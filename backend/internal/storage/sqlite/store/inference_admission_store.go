package store

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RequeueUndispatchedTurns preserves authored text, structured deliveries and
// original order after a final-boundary guard proves zero provider bytes sent.
// A bound or completed turn is never eligible, even if a caller is mistaken.
func (s *Store) RequeueUndispatchedTurns(ctx context.Context, conversationID string, ids []string, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "requeue refused inference", func(q *gen.Queries) error {
		for _, id := range ids {
			n, err := q.RequeueUndispatchedConversationTurn(ctx, gen.RequeueUndispatchedConversationTurnParams{ID: id, ConversationID: conversationID, Reason: reason})
			if err := expectBatchRow("requeue undispatched turn", id, n, err); err != nil {
				return err
			}
		}
		return nil
	})
}
