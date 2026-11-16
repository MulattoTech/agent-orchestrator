package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// This double records the provider write before returning an unknown outcome.
// No executable, provider daemon, credentials or network is involved.
type acceptedThenUncertainReportConversation struct{ *fakeConversation }

func (c *acceptedThenUncertainReportConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if _, err := c.fakeConversation.SendTurn(ctx, msg); err != nil {
		return ports.ChatTurnRef{}, err
	}
	return ports.ChatTurnRef{}, fmt.Errorf("%w: synthetic accepted write lost its reply", inferenceguard.ErrUncertain)
}

func restartReportService(t *testing.T, h *harness) (*chatsvc.Service, *fakeConversation) {
	t.Helper()
	ctx := context.Background()
	if err := h.svc.Stop(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	resumed := newFakeConversation()
	resumed.turnSeq = 100
	var seq atomic.Int64
	svc := chatsvc.New(chatsvc.Options{
		Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), PageReader: boundedSnapshotReader(h.st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: resumed}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return fmt.Sprintf("report-resume-%d", seq.Add(1)) },
		Now:     h.now, DataDir: h.rendersDir, Renders: h.renders,
	})
	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", HistoryMode: ports.ChatHistoryDeferred,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.ConversationID() != h.ctrl.ConversationID() {
		t.Fatalf("restart replaced durable conversation: %s", ctrl.ConversationID())
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	return svc, resumed
}

func TestReportPiggybackQuotaAcceptanceCancelRequeueAndNoDuplicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPiggybackReport(t, h, "held-piggyback")
	h.conv.mu.Lock()
	h.conv.sendErr = &inferenceguard.Refusal{Code: inferenceguard.ErrQuota, Reason: "synthetic exhausted account"}
	h.conv.mu.Unlock()
	original, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "first authored request", ClientMessageID: "held-original"})
	if !errors.Is(err, inferenceguard.ErrQuota) || original.State != domain.TurnStateQueued {
		t.Fatalf("original=%+v %v", original, err)
	}
	before, found, err := h.st.GetReport(ctx, "held-piggyback")
	if err != nil || !found || before.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("report not durably accepted: %+v %v", before, err)
	}
	if err := h.svc.CancelQueuedTurn(ctx, testSession, original.ID); err != nil {
		t.Fatal(err)
	}
	withdrawn, _, _ := h.st.GetReport(ctx, "held-piggyback")
	if withdrawn.DeliveryState != domain.ReportPending {
		t.Fatalf("withdrawn report=%+v", withdrawn)
	}
	h.advance(time.Second)
	replacement, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "replacement authored request", ClientMessageID: "held-replacement"})
	if !errors.Is(err, inferenceguard.ErrQuota) || replacement.State != domain.TurnStateQueued {
		t.Fatalf("replacement=%+v %v", replacement, err)
	}
	accepted, _, _ := h.st.GetReport(ctx, "held-piggyback")
	if accepted.DeliveryState != domain.ReportAcknowledged || accepted.DeliveryBatchID != before.DeliveryBatchID || accepted.Note != before.Note {
		t.Fatalf("reaccepted report=%+v", accepted)
	}
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "replacement authored request", ClientMessageID: "held-replacement"}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "later authored request", ClientMessageID: "held-later"}); err != nil && !errors.Is(err, inferenceguard.ErrQuota) {
		t.Fatal(err)
	}
	queued, err := h.st.ListQueuedBatch(ctx, h.ctrl.ConversationID(), 0)
	if err != nil || len(queued) != 2 {
		t.Fatalf("queued=%+v %v", queued, err)
	}
	count := 0
	for _, q := range queued {
		count += strings.Count(q.DeliveryContentJSON, "fixture report held-piggyback")
	}
	if count != 1 {
		t.Fatalf("same report attached %d times", count)
	}
	if len(h.conv.sentMessages()) != 0 {
		t.Fatal("quota hold wrote to provider")
	}
	h.conv.mu.Lock()
	h.conv.sendErr = nil
	h.conv.mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := h.svc.DrainQueued(ctx, testSession); err != nil {
			t.Fatal(err)
		}
	}
	sent := h.conv.sentMessages()
	if len(sent) != 1 || strings.Count(sent[0].Text, "fixture report held-piggyback") != 1 || !strings.HasPrefix(sent[0].Text, "replacement authored request\n\n") {
		t.Fatalf("controlled resume=%+v", sent)
	}
	later, found, err := h.st.ConversationMessageByClientID(ctx, h.ctrl.ConversationID(), "held-later")
	if err != nil || !found || later.DeliveryContentJSON != "" {
		t.Fatalf("later request duplicated context: %+v %v", later, err)
	}
}

type reportServiceDelivery struct {
	svc    *chatsvc.Service
	ids    []string
	errors []error
}

func (d *reportServiceDelivery) Submit(ctx context.Context, id domain.SessionID, text, key string) error {
	turn, err := d.svc.RelaySessionChatTurn(ctx, id, text, key, ports.MessageDeliveryOptions{ReportDelivery: true})
	d.ids = append(d.ids, turn)
	d.errors = append(d.errors, err)
	return err
}
func (d *reportServiceDelivery) Interrupt(context.Context, domain.SessionID) error { return nil }

func TestStandaloneReportGuardAcceptanceSurvivesRestart(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "quota-before-write"
		if uncertain {
			name = "accepted-write-then-uncertain"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var h *harness
			if uncertain {
				provider := &acceptedThenUncertainReportConversation{newFakeConversation()}
				h = newHarnessWithConversation(t, provider)
				h.conv = provider.fakeConversation
			} else {
				h = newHarness(t)
				h.conv.mu.Lock()
				h.conv.sendErr = &inferenceguard.Refusal{Code: inferenceguard.ErrQuota, Reason: "synthetic exhausted account"}
				h.conv.mu.Unlock()
			}
			seedPiggybackReport(t, h, "standalone-guard")
			delivery := &reportServiceDelivery{svc: h.svc}
			coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Delivery: delivery, Now: h.now})
			h.advance(time.Hour)
			_, err := coordinator.RunDue(ctx)
			cause := inferenceguard.ErrQuota
			if uncertain {
				cause = inferenceguard.ErrUncertain
			}
			var accepted *domain.ReportDeliveryAcceptedError
			if !errors.Is(err, cause) || !errors.As(err, &accepted) || accepted.TurnID == "" {
				t.Fatalf("lost durable receipt or original failure: %v", err)
			}
			report, _, err := h.st.GetReport(ctx, "standalone-guard")
			if err != nil || report.DeliveryState != domain.ReportAcknowledged || accepted.DeliveryID != report.DeliveryBatchID {
				t.Fatalf("report=%+v %v", report, err)
			}
			message, found, err := h.st.ConversationMessageByClientID(ctx, h.ctrl.ConversationID(), report.DeliveryBatchID)
			if err != nil || !found || message.TurnID != accepted.TurnID || message.Origin != domain.MessageOriginAutomation || strings.Count(message.Text, "fixture report standalone-guard") != 1 {
				t.Fatalf("durable message=%+v %v", message, err)
			}
			originalText := message.Text
			stored, err := h.st.TurnByID(ctx, message.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.TurnStateQueued
			if uncertain {
				want = domain.TurnStateFailed
			}
			if stored.State != want || (uncertain && stored.ProviderInputText != originalText) {
				t.Fatalf("durable turn=%+v", stored)
			}
			writes := len(h.conv.sentMessages())
			if uncertain && writes != 1 || !uncertain && writes != 0 {
				t.Fatalf("provider writes=%d", writes)
			}
			if _, err := coordinator.RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			if len(delivery.ids) != 1 {
				t.Fatalf("coordinator resubmitted accepted batch: %v", delivery.ids)
			}
			restarted, provider := restartReportService(t, h)
			if len(provider.sentMessages()) != 0 {
				t.Fatal("restart automatically resubmitted report")
			}
			again := &reportServiceDelivery{svc: restarted}
			recovered := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Delivery: again, Now: h.now})
			if _, err := recovered.RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			if len(again.ids) != 0 {
				t.Fatal("acknowledged batch reclaimed after restart")
			}
			for i := 0; i < 2; i++ {
				if err := restarted.DrainQueued(ctx, testSession); err != nil && !errors.Is(err, domain.ErrNoQueuedTurn) {
					t.Fatal(err)
				}
			}
			sent := provider.sentMessages()
			if uncertain {
				if len(sent) != 0 {
					t.Fatalf("uncertain original report resent: %+v", sent)
				}
			} else if len(sent) != 1 || sent[0].Text != originalText {
				t.Fatalf("controlled resume=%+v", sent)
			}
			if _, err := restarted.RelaySessionChatTurn(ctx, testSession, originalText, report.DeliveryBatchID, ports.MessageDeliveryOptions{ReportDelivery: true}); err != nil {
				t.Fatal(err)
			}
			if len(provider.sentMessages()) != len(sent) {
				t.Fatal("same delivery id created second native write")
			}
			messages, err := h.st.ConversationMessages(ctx, h.ctrl.ConversationID())
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, m := range messages {
				if m.ClientMessageID == report.DeliveryBatchID {
					count++
					if m.Text != originalText || m.TurnID != accepted.TurnID {
						t.Fatal("original batch/turn changed")
					}
				}
			}
			if count != 1 {
				t.Fatalf("durable report messages=%d", count)
			}
		})
	}
}

func TestReportPiggybackUncertainWriteRetainsOriginalAndNeverReclaims(t *testing.T) {
	provider := &acceptedThenUncertainReportConversation{newFakeConversation()}
	h := newHarnessWithConversation(t, provider)
	h.conv = provider.fakeConversation
	seedPiggybackReport(t, h, "uncertain-piggyback")
	ctx := context.Background()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "authored request", ClientMessageID: "uncertain-human"})
	if !errors.Is(err, inferenceguard.ErrUncertain) || turn.State != domain.TurnStateFailed {
		t.Fatalf("ordinary caller lost uncertain error: %+v %v", turn, err)
	}
	report, _, err := h.st.GetReport(ctx, "uncertain-piggyback")
	if err != nil || report.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("report=%+v %v", report, err)
	}
	sent := provider.sentMessages()
	if len(sent) != 1 || strings.Count(sent[0].Text, "fixture report uncertain-piggyback") != 1 {
		t.Fatalf("accepted fake writes=%+v", sent)
	}
	svc, resumed := restartReportService(t, h)
	coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now})
	svc.SetReportCoordinator(coordinator)
	if err := svc.DrainQueued(ctx, testSession); err != nil && !errors.Is(err, domain.ErrNoQueuedTurn) {
		t.Fatal(err)
	}
	if len(resumed.sentMessages()) != 0 {
		t.Fatal("restart repeated an uncertain provider write")
	}
	if _, err := svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "new independent request", ClientMessageID: "new-after-uncertain"}); err != nil {
		t.Fatal(err)
	}
	sent = resumed.sentMessages()
	if len(sent) != 1 || sent[0].Text != "new independent request" {
		t.Fatalf("later user received repeated context: %+v", sent)
	}
	original, found, err := h.st.ConversationMessageByClientID(ctx, h.ctrl.ConversationID(), "uncertain-human")
	if err != nil || !found || original.TurnID != turn.ID || original.Text != "authored request" || strings.Count(original.DeliveryContentJSON, "fixture report uncertain-piggyback") != 1 {
		t.Fatalf("original content lost: %+v %v", original, err)
	}
	after, _, _ := h.st.GetReport(ctx, "uncertain-piggyback")
	if after.DeliveryState != domain.ReportAcknowledged || after.DeliveryBatchID != report.DeliveryBatchID || after.DeliveryAttempts != report.DeliveryAttempts {
		t.Fatalf("batch reclaimed: %+v", after)
	}
}

func TestStandaloneReportSurvivesHandoffBulkCancelAndControlledResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "active ordinary task", ClientMessageID: "handoff-active"}); err != nil {
		t.Fatal(err)
	}
	seedPiggybackReport(t, h, "handoff-report")
	reports, err := h.st.ClaimPendingReportBatch(ctx, testProject, "handoff-claim", h.now())
	if err != nil || len(reports) != 1 {
		t.Fatalf("claim=%+v %v", reports, err)
	}
	batch := reportsvc.PreparedBatch{ProjectID: testProject, Token: "handoff-claim", ID: reports[0].DeliveryBatchID, Reports: reports}
	originalText := batch.Render()
	reportTurn, err := h.svc.RelaySessionChatTurn(ctx, testSession, originalText, batch.IdempotencyKey(), ports.MessageDeliveryOptions{ReportDelivery: true})
	if err != nil || reportTurn == "" {
		t.Fatalf("queue report=%q %v", reportTurn, err)
	}
	if n, err := h.st.AcknowledgeReportBatch(ctx, testProject, batch.Token, h.now()); err != nil || n != 1 {
		t.Fatalf("ack=%d %v", n, err)
	}
	h.advance(time.Second)
	ordinary, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "ordinary queued command", ClientMessageID: "handoff-command"})
	if err != nil {
		t.Fatal(err)
	}
	forged, err := h.svc.RelaySessionChatTurn(ctx, testSession, "prefix is not durable report proof", "report-batch:forged", ports.MessageDeliveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.PrepareChatHandoff(ctx, testSession, domain.SessionInterfaceTransitionInterrupt); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		id   string
		want domain.TurnState
	}{{reportTurn, domain.TurnStateQueued}, {ordinary.ID, domain.TurnStateInterrupted}, {forged, domain.TurnStateInterrupted}} {
		turn, err := h.st.TurnByID(ctx, check.id)
		if err != nil || turn.State != check.want {
			t.Fatalf("turn %s=%+v %v", check.id, turn, err)
		}
	}
	report, _, _ := h.st.GetReport(ctx, "handoff-report")
	if report.DeliveryState != domain.ReportAcknowledged {
		t.Fatal("handoff reset accepted report ACK")
	}
	if len(h.conv.sentMessages()) != 1 {
		t.Fatal("handoff dispatched report")
	}
	// The fake provider acknowledges the interrupt. Completion must not drain
	// the accepted report while the interface handoff fence is armed.
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateInterrupted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		states := turnStateByText(t, s)
		return states["active ordinary task"] == domain.TurnStateInterrupted && states[originalText] == domain.TurnStateQueued
	})
	svc, resumed := restartReportService(t, h)
	if len(resumed.sentMessages()) != 0 {
		t.Fatal("restart bypassed controlled queue resume")
	}
	for i := 0; i < 2; i++ {
		if err := svc.DrainQueued(ctx, testSession); err != nil {
			t.Fatal(err)
		}
	}
	sent := resumed.sentMessages()
	if len(sent) != 1 || sent[0].Text != originalText {
		t.Fatalf("resumed report=%+v", sent)
	}
	row, err := h.st.TurnByID(ctx, reportTurn)
	if err != nil || row.ProviderTurnID == "" {
		t.Fatalf("lost original report turn: %+v %v", row, err)
	}
	if _, err := svc.RelaySessionChatTurn(ctx, testSession, originalText, batch.IdempotencyKey(), ports.MessageDeliveryOptions{ReportDelivery: true}); err != nil {
		t.Fatal(err)
	}
	if len(resumed.sentMessages()) != 1 {
		t.Fatal("same batch replay created second write")
	}
}

func TestOrdinaryRelayRetainsGuardErrorWithoutReportAcceptance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.conv.mu.Lock()
	h.conv.sendErr = &inferenceguard.Refusal{Code: inferenceguard.ErrQuota, Reason: "synthetic quota"}
	h.conv.mu.Unlock()
	id, err := h.svc.RelaySessionChatTurn(ctx, testSession, "ordinary automation", "ordinary-relay", ports.MessageDeliveryOptions{})
	var accepted *domain.ReportDeliveryAcceptedError
	if id == "" || !errors.Is(err, inferenceguard.ErrQuota) || errors.As(err, &accepted) {
		t.Fatalf("ordinary relay acceptance changed: %q %v", id, err)
	}
}

func TestStandaloneReportExplicitStopStillCancelsQueuedTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "running", ClientMessageID: "stop-active"}); err != nil {
		t.Fatal(err)
	}
	seedPiggybackReport(t, h, "stop-report")
	reports, err := h.st.ClaimPendingReportBatch(ctx, testProject, "stop-claim", h.now())
	if err != nil || len(reports) != 1 {
		t.Fatalf("claim=%+v %v", reports, err)
	}
	batch := reportsvc.PreparedBatch{ProjectID: testProject, Token: "stop-claim", ID: reports[0].DeliveryBatchID, Reports: reports}
	id, err := h.svc.RelaySessionChatTurn(ctx, testSession, batch.Render(), batch.IdempotencyKey(), ports.MessageDeliveryOptions{ReportDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AcknowledgeReportBatch(ctx, testProject, batch.Token, h.now()); err != nil {
		t.Fatal(err)
	}
	// This is the store operation used by an explicit user Stop. The interface
	// handoff exception must not override the operator's cancellation intent.
	if err := h.st.CancelQueuedTurns(ctx, h.ctrl.ConversationID(), h.now(), h.now()); err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.TurnByID(ctx, id)
	if err != nil || !turn.State.Terminal() {
		t.Fatalf("operator stop did not cancel: %+v %v", turn, err)
	}
	report, _, _ := h.st.GetReport(ctx, "stop-report")
	if report.DeliveryState != domain.ReportAcknowledged {
		t.Fatal("explicit stop silently rescheduled the report")
	}
}

func TestFortyHeldHumanMessagesAndReportSurviveControllerCrashRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.conv.mu.Lock()
	h.conv.sendErr = &inferenceguard.Refusal{Code: inferenceguard.ErrQuota, Reason: "synthetic exhausted account"}
	h.conv.mu.Unlock()
	var ids []string
	for i := 0; i < 40; i++ {
		turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: fmt.Sprintf("durable-task-%02d-end", i), ClientMessageID: fmt.Sprintf("durable-human-%02d", i)})
		if !errors.Is(err, inferenceguard.ErrQuota) || turn.State != domain.TurnStateQueued {
			t.Fatalf("human %d=%+v %v", i, turn, err)
		}
		ids = append(ids, turn.ID)
		h.advance(time.Second)
	}
	seedPiggybackReport(t, h, "forty-report")
	delivery := &reportServiceDelivery{svc: h.svc}
	coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Delivery: delivery, Now: h.now})
	h.advance(time.Hour)
	if _, err := coordinator.RunDue(ctx); !errors.Is(err, inferenceguard.ErrQuota) {
		t.Fatalf("report acceptance=%v", err)
	}
	before, err := h.st.ListQueuedBatch(ctx, h.ctrl.ConversationID(), 0)
	if err != nil || len(before) != 41 {
		t.Fatalf("before restart queue=%d %v", len(before), err)
	}
	if len(h.conv.sentMessages()) != 0 {
		t.Fatal("quota hold made provider writes")
	}
	// Model an unexpected provider stream loss, then normal controller teardown
	// and startup against the same real disposable SQLite store.
	if err := h.conv.Close(); err != nil {
		t.Fatal(err)
	}
	svc, resumed := restartReportService(t, h)
	after, err := h.st.ListQueuedBatch(ctx, h.ctrl.ConversationID(), 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("restart queue=%d %v", len(after), err)
	}
	for i, q := range after {
		if q.TurnID != before[i].TurnID || q.Text != before[i].Text || q.DeliveryContentJSON != before[i].DeliveryContentJSON {
			t.Fatalf("restart mutated queue item %d", i)
		}
	}
	if len(resumed.sentMessages()) != 0 {
		t.Fatal("restart bypassed controlled resume")
	}
	for i := 0; i < 2; i++ {
		if err := svc.DrainQueued(ctx, testSession); err != nil {
			t.Fatal(err)
		}
	}
	if sent := resumed.sentMessages(); len(sent) != 1 || sent[0].Text != "durable-task-00-end" {
		t.Fatalf("first controlled resume=%+v", sent)
	}
	for step := 0; step < 41; step++ {
		sent := resumed.sentMessages()
		if strings.Contains(sent[len(sent)-1].Text, "fixture report forty-report") {
			break
		}
		count := len(sent)
		resumed.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: fmt.Sprintf("provider-turn-%d", 100+count), TurnState: domain.TurnStateCompleted})
		h.awaitSnapshot(t, func(store.ConversationSnapshot) bool { return len(resumed.sentMessages()) > count })
	}
	var all strings.Builder
	for _, m := range resumed.sentMessages() {
		all.WriteString(m.Text)
		all.WriteString("\n")
	}
	for i := 0; i < 40; i++ {
		if count := strings.Count(all.String(), fmt.Sprintf("durable-task-%02d-end", i)); count != 1 {
			t.Fatalf("human %d delivered %d times", i, count)
		}
	}
	if count := strings.Count(all.String(), "fixture report forty-report"); count != 1 {
		t.Fatalf("report delivered %d times", count)
	}
	report, _, _ := h.st.GetReport(ctx, "forty-report")
	if report.DeliveryState != domain.ReportAcknowledged || report.DeliveryAttempts != 1 {
		t.Fatalf("report duplicated/reclaimed: %+v", report)
	}
	for _, id := range ids {
		if _, err := h.st.TurnByID(ctx, id); err != nil {
			t.Fatalf("original human turn disappeared: %s %v", id, err)
		}
	}
}

type guardedUncertainReportConversation struct {
	*fakeConversation
	gate    *inferenceguard.Gate
	request inferenceguard.Request
}

func (c *guardedUncertainReportConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	err := c.gate.Dispatch(ctx, c.request, func() error {
		if _, err := c.fakeConversation.SendTurn(ctx, msg); err != nil {
			return err
		}
		return errors.New("synthetic provider accepted write; response lost")
	})
	return ports.ChatTurnRef{}, err
}

func TestUncertainReportKeepsGlobalAdmissionLeaseAfterRecovery(t *testing.T) {
	provider := &guardedUncertainReportConversation{fakeConversation: newFakeConversation()}
	h := newHarnessWithConversation(t, provider)
	h.conv = provider.fakeConversation
	policy := inferenceguard.Policy{
		Version: 1, AccountKey: "synthetic-account", AuthorizationEpoch: "synthetic-window",
		MaximumEffort: "medium", MaximumInference: 2, MaximumPerProject: 2, MaximumReviewers: 1,
		TurnBudget: 10, ReserveTurns: 1, Roles: map[string]inferenceguard.RolePolicy{},
	}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		policy.Roles[role] = inferenceguard.RolePolicy{DefaultModel: "synthetic-model", Models: []string{"synthetic-model"}}
	}
	path := filepath.Join(t.TempDir(), "admission.db")
	loader := func() (inferenceguard.Policy, error) { return policy, nil }
	gate, err := inferenceguard.Open(path, loader, h.now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close() }()
	selection, err := gate.Resolve("orchestrator", string(testSession), "synthetic-model", "medium")
	if err != nil {
		t.Fatal(err)
	}
	provider.gate = gate
	provider.request = inferenceguard.Request{ID: "synthetic-report-dispatch", Session: string(testSession), Project: string(testProject), Operation: "turn/start", Selection: selection}
	seedPiggybackReport(t, h, "lease-report")
	ctx := context.Background()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "preserve uncertain admission", ClientMessageID: "lease-human"})
	if !errors.Is(err, inferenceguard.ErrUncertain) || turn.State != domain.TurnStateFailed {
		t.Fatalf("turn=%+v %v", turn, err)
	}
	snapshot, err := gate.Snapshot(ctx)
	if err != nil || snapshot.Uncertain != 1 || len(provider.sentMessages()) != 1 {
		t.Fatalf("lost uncertain lease: %+v %v", snapshot, err)
	}
	report, _, _ := h.st.GetReport(ctx, "lease-report")
	if report.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("batch released after unknown write: %+v", report)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	gate, err = inferenceguard.Open(path, loader, h.now)
	if err != nil {
		t.Fatal(err)
	}
	provider.gate = gate
	svc, resumed := restartReportService(t, h)
	if err := svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if len(resumed.sentMessages()) != 0 {
		t.Fatal("restart resent an uncertain report")
	}
	if err := gate.Dispatch(ctx, provider.request, func() error { t.Fatal("duplicate uncertain request crossed provider boundary"); return nil }); !errors.Is(err, inferenceguard.ErrUncertain) {
		t.Fatal(err)
	}
	snapshot, err = gate.Snapshot(ctx)
	if err != nil || snapshot.Uncertain != 1 {
		t.Fatalf("recovery released uncertain admission: %+v %v", snapshot, err)
	}
}
