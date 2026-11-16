package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

func seedPiggybackReport(t *testing.T, h *harness, id string) {
	t.Helper()
	ctx := context.Background()
	worker, err := h.st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: testProject, Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
		CreatedAt: h.now(), UpdatedAt: h.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.st.CreateReport(ctx, domain.ReportRecord{
		ID: id, ProjectID: testProject, SessionID: worker.ID,
		State: domain.ReportCheckpoint, Note: "fixture report " + id,
		CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour),
		DeliveryState: domain.ReportPending, RepeatCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{
		Store: h.st, Now: h.now,
	}))
}

func TestReportContextSurvivesQueuedEditAndSteerWithoutEnteringAuthoredText(t *testing.T) {
	h, provider := steerHarness(t)
	seedPiggybackReport(t, h, "queued-report")
	ctx := context.Background()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "authored request", Origin: domain.MessageOriginHuman, ClientMessageID: "report-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.st.QueuedTurnMessage(ctx, h.ctrl.ConversationID(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text != "authored request" {
		t.Fatalf("report leaked into authored text: %q", msg.Text)
	}
	var content []ports.ChatContent
	if err := json.Unmarshal([]byte(msg.DeliveryContentJSON), &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 1 || !content[0].Internal || !strings.HasPrefix(content[0].URI, ports.ChatWorkerReportURIPrefix) {
		t.Fatalf("report context is not a structured internal receipt: %+v", content)
	}
	zero := int64(0)
	edit := chatsvc.QueuedMessageEdit{Text: "edited request", RetainedContent: &[]int{}, ExpectedRevision: &zero, ClientMessageID: "report-edit"}
	if err := h.svc.EditQueuedTurn(ctx, testSession, turn.ID, edit); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.EditQueuedTurn(ctx, testSession, turn.ID, edit); err != nil {
		t.Fatal(err)
	}
	edited, err := h.st.QueuedTurnMessage(ctx, h.ctrl.ConversationID(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Text != "edited request" || edited.DeliveryContentJSON != msg.DeliveryContentJSON {
		t.Fatalf("edit changed server context or authored text: %+v", edited)
	}
	if _, err := h.svc.PromoteQueuedTurn(ctx, testSession, turn.ID); err != nil {
		t.Fatal(err)
	}
	calls := provider.steers()
	if len(calls) != 1 || strings.Count(calls[0].msg.Text, "fixture report queued-report") != 1 || !strings.HasPrefix(calls[0].msg.Text, "edited request\n\n") {
		t.Fatalf("provider did not receive one report with edited text: %+v", calls)
	}
	if len(calls[0].msg.Content) != 0 {
		t.Fatalf("internal text was serialized twice: %+v", calls[0].msg.Content)
	}
}

func TestReportContextRetryUsesFrozenInputExactlyOnce(t *testing.T) {
	h := newHarness(t)
	seedPiggybackReport(t, h, "retry-report")
	ctx := context.Background()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "authored retry request", Origin: domain.MessageOriginHuman, ClientMessageID: "report-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID,
		TurnState: domain.TurnStateFailed, Err: errors.New("synthetic failure")})
	failedTurnSnapshot(t, h, turn.ID)
	retry, err := h.svc.RetryTurn(ctx, testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := h.svc.RetryTurn(ctx, testSession, turn.ID)
	if err != nil || duplicate.ID != retry.ID {
		t.Fatalf("duplicate retry: %+v %v", duplicate, err)
	}
	sent := h.conv.sentMessages()
	if len(sent) != 2 || sent[0].Text != sent[1].Text || strings.Count(sent[1].Text, "fixture report retry-report") != 1 {
		t.Fatalf("retry duplicated or changed provider context: %+v", sent)
	}
	snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range snapshot.Messages {
		if msg.Role == domain.MessageRoleUser && msg.Text != "authored retry request" {
			t.Fatalf("retry exposed provider text to the editor: %q", msg.Text)
		}
	}
	report, found, err := h.st.GetReport(ctx, "retry-report")
	if err != nil || !found || report.Note != "fixture report retry-report" {
		t.Fatalf("durable report lost: %+v %v", report, err)
	}
}

func TestCancelledQueuedReportIsRequeuedAtomically(t *testing.T) {
	for _, mode := range []string{"one", "stop", "handoff"} {
		t.Run(mode, func(t *testing.T) {
			h, provider := steerHarness(t)
			seedPiggybackReport(t, h, "cancel-report")
			ctx := context.Background()
			turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "withdraw me", Origin: domain.MessageOriginHuman, ClientMessageID: "cancel-message"})
			if err != nil {
				t.Fatal(err)
			}
			report, _, err := h.st.GetReport(ctx, "cancel-report")
			if err != nil || report.DeliveryState != domain.ReportAcknowledged {
				t.Fatalf("accepted report: %+v %v", report, err)
			}
			before := report
			switch mode {
			case "one":
				err = h.svc.CancelQueuedTurn(ctx, testSession, turn.ID)
			case "stop":
				err = h.st.CancelQueuedTurns(ctx, h.ctrl.ConversationID(), h.now(), h.now())
			case "handoff":
				err = h.st.CancelAllQueuedTurns(ctx, h.ctrl.ConversationID(), h.now())
			}
			if err != nil {
				t.Fatal(err)
			}
			report, _, err = h.st.GetReport(ctx, "cancel-report")
			if err != nil || report.DeliveryState != domain.ReportPending || report.Note != before.Note || report.DeliveryBatchID != before.DeliveryBatchID {
				t.Fatalf("withdrawn context lost or payload changed: %+v %v", report, err)
			}
			if len(provider.steers()) != 0 {
				t.Fatal("cancellation dispatched provider work")
			}
			if mode == "one" {
				if err := h.svc.CancelQueuedTurn(ctx, testSession, turn.ID); err == nil {
					t.Fatal("duplicate cancellation should be refused")
				}
			}
			claimed, err := h.st.ClaimPendingReportBatch(ctx, testProject, "new-claim", h.now())
			if err != nil || len(claimed) != 1 {
				t.Fatalf("requeue not reusable: %+v %v", claimed, err)
			}
			if n, err := h.st.AcknowledgeReportBatch(ctx, testProject, "new-claim", h.now()); err != nil || n != 1 {
				t.Fatalf("reaccept=%d %v", n, err)
			}
			// A later bulk Stop must not revive reports from old cancelled rows.
			if err := h.st.CancelAllQueuedTurns(ctx, h.ctrl.ConversationID(), h.now()); err != nil {
				t.Fatal(err)
			}
			report, _, _ = h.st.GetReport(ctx, "cancel-report")
			if report.DeliveryState != domain.ReportAcknowledged {
				t.Fatal("old cancelled row replayed an already accepted report")
			}
		})
	}
}
