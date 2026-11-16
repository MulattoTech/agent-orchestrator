package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

const legacyReportEnvelope = "\n\n<ao-worker-reports>\nlegacy preserved report\n</ao-worker-reports>"

func TestLegacyReportContextSurvivesQueuedEdit(t *testing.T) {
	h, provider := steerHarness(t)
	ctx := context.Background()
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "old authored text" + legacyReportEnvelope, Origin: domain.MessageOriginHuman, ClientMessageID: "legacy-queued"})
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	// Also handle an old client resending the hidden suffix with an edited draft.
	edit := chatsvc.QueuedMessageEdit{Text: "new authored text" + legacyReportEnvelope, RetainedContent: &[]int{}, ExpectedRevision: &revision, ClientMessageID: "legacy-edit"}
	if err := h.svc.EditQueuedTurn(ctx, testSession, turn.ID, edit); err != nil {
		t.Fatal(err)
	}
	msg, err := h.st.QueuedTurnMessage(ctx, h.ctrl.ConversationID(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text != "new authored text" || !strings.Contains(msg.DeliveryContentJSON, "legacy preserved report") {
		t.Fatalf("legacy migration=%+v", msg)
	}
	if _, err := h.svc.PromoteQueuedTurn(ctx, testSession, turn.ID); err != nil {
		t.Fatal(err)
	}
	calls := provider.steers()
	if len(calls) != 1 || calls[0].msg.Text != "new authored text"+legacyReportEnvelope {
		t.Fatalf("legacy provider context=%+v", calls)
	}
}
func TestLegacyReportContextRetryKeepsAuthoredTextSeparate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "old authored text" + legacyReportEnvelope, Origin: domain.MessageOriginHuman, ClientMessageID: "legacy-retry"})
	if err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateFailed, Err: errors.New("synthetic failure")})
	failedTurnSnapshot(t, h, turn.ID)
	retry, err := h.svc.RetryTurn(ctx, testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range snapshot.Messages {
		if msg.TurnID == retry.ID && msg.Role == domain.MessageRoleUser {
			found = true
			if msg.Text != "old authored text" || !strings.Contains(msg.DeliveryContentJSON, "legacy preserved report") {
				t.Fatalf("retry authored/context=%+v", msg)
			}
		}
	}
	if !found {
		t.Fatal("retry message missing")
	}
	calls := h.conv.sentMessages()
	if len(calls) != 2 || calls[0].Text != calls[1].Text {
		t.Fatalf("legacy retry changed provider input: %+v", calls)
	}
}
