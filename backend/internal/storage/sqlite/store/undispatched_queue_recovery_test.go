package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestOrphanCleanupPreservesOnlyProvenUndispatchedQueue(t *testing.T) {
	for _, kind := range []string{"undispatched", "frozen-input", "bound", "reserved", "running"} {
		t.Run(kind, func(t *testing.T) {
			s, session, conversation := conversationFixture(t)
			ctx := context.Background()
			created, err := s.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{
				ID: "message", Text: "durable command", Origin: domain.MessageOriginHuman, ClientMessageID: "client",
			}, "turn", histClock)
			if err != nil || !created {
				t.Fatalf("append=%v %v", created, err)
			}
			switch kind {
			case "frozen-input":
				err = s.RecordProviderInput(ctx, "turn", "provider input could have crossed the boundary")
			case "bound":
				err = s.BindTurnToProvider(ctx, "turn", "native-turn", histClock)
			case "reserved":
				_, err = s.ReserveQueuedTurnForPromotion(ctx, conversation, "turn", histClock)
			case "running":
				err = s.MarkTurnDispatching(ctx, "turn")
			}
			if err != nil {
				t.Fatal(err)
			}
			if owned, err := s.CleanupOwnedControllerWork(ctx, session, conversation, "gen-1", histClock.Add(time.Minute)); err != nil || !owned {
				t.Fatalf("cleanup=%v %v", owned, err)
			}
			turn, err := s.TurnByID(ctx, "turn")
			want := domain.TurnStateFailed
			if kind == "undispatched" {
				want = domain.TurnStateQueued
			}
			if err != nil || turn.State != want {
				t.Fatalf("turn=%+v %v", turn, err)
			}
			if kind == "undispatched" {
				if err := s.CancelQueuedTurns(ctx, conversation, histClock.Add(time.Minute), histClock.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				turn, err = s.TurnByID(ctx, "turn")
				if err != nil || !turn.State.Terminal() {
					t.Fatalf("explicit stop ignored: %+v %v", turn, err)
				}
			}
		})
	}
}
