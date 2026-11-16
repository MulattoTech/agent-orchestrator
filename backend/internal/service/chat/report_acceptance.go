package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

func validReportDeliveryID(id string) bool {
	parsed, ok := domain.ReportDeliveryID(domain.WrapReportDelivery(id, ""))
	return ok && parsed == id && strings.HasPrefix(id, "report-batch:")
}

// acceptedReportMessage verifies persisted facts, not merely the controller's
// returned turn id. A proved refusal can be queued. An uncertain provider write
// must remain failed, with its frozen input preserved, and must never be drained.
func (s *Service) acceptedReportMessage(ctx context.Context, turn domain.ConversationTurn, msg ports.ChatUserMessage, cause error) (domain.ConversationTurn, domain.ConversationMessage, bool) {
	if turn.ID == "" || turn.ConversationID == "" {
		return domain.ConversationTurn{}, domain.ConversationMessage{}, false
	}
	queued := turn.State == domain.TurnStateQueued && inferenceguard.PreDispatchRefusal(cause) && !errors.Is(cause, inferenceguard.ErrUncertain)
	uncertain := turn.State == domain.TurnStateFailed && errors.Is(cause, inferenceguard.ErrUncertain)
	if !queued && !uncertain {
		return domain.ConversationTurn{}, domain.ConversationMessage{}, false
	}
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stored, err := s.store.TurnByID(proofCtx, turn.ID)
	if err != nil || stored.ConversationID != turn.ConversationID || stored.State != turn.State ||
		stored.HandledBySessionID != turn.HandledBySessionID || stored.RolledBackAt != nil ||
		(uncertain && stored.ProviderInputText == "") {
		return domain.ConversationTurn{}, domain.ConversationMessage{}, false
	}
	var message domain.ConversationMessage
	var found bool
	if msg.ClientMessageID != "" {
		message, found, err = s.store.ConversationMessageByClientID(proofCtx, turn.ConversationID, msg.ClientMessageID)
	} else {
		// Legacy callers may omit a client id. Use the exact durable turn rather
		// than treating the absence of an id as permission to release its reports.
		var messages []domain.ConversationMessage
		messages, err = s.store.ConversationMessages(proofCtx, turn.ConversationID)
		for _, candidate := range messages {
			if candidate.TurnID == turn.ID && candidate.Role == domain.MessageRoleUser {
				message = candidate
				found = true
				break
			}
		}
	}
	if err != nil || !found || message.TurnID != turn.ID || message.ConversationID != turn.ConversationID ||
		message.ClientMessageID != msg.ClientMessageID || message.Role != domain.MessageRoleUser {
		return domain.ConversationTurn{}, domain.ConversationMessage{}, false
	}
	return stored, message, true
}

func (s *Service) acceptedPiggyback(ctx context.Context, turn domain.ConversationTurn, msg ports.ChatUserMessage, batch reportsvc.PreparedBatch, cause error) bool {
	if len(batch.Reports) == 0 {
		return false
	}
	stored, message, ok := s.acceptedReportMessage(ctx, turn, msg, cause)
	if !ok || message.Origin == domain.MessageOriginAutomation {
		return false
	}
	var blocks []ports.ChatContent
	if err := json.Unmarshal([]byte(message.DeliveryContentJSON), &blocks); err != nil {
		return false
	}
	context := batch.ProviderContext()
	matches := 0
	for _, block := range blocks {
		if block.Type == "text" && block.Internal && block.URI == ports.ChatWorkerReportURIPrefix+batch.IdempotencyKey() && block.Text == context {
			matches++
		}
	}
	return matches == 1 && (stored.State == domain.TurnStateQueued || strings.Count(stored.ProviderInputText, context) == 1)
}
