package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestRequeueUndispatchedBatchRetainsOrderAndIsAtomic(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("guard-%02d", i)
		ids = append(ids, id)
		_, err := s.AppendUserMessage(ctx, conversation, session, "gen-1",
			domain.ConversationMessage{ID: "message-" + id, Text: "authored " + id, ClientMessageID: id, Origin: domain.MessageOriginHuman,
				DeliveryContentJSON: "[{\"type\":\"text\",\"internal\":true,\"text\":\"report\",\"uri\":\"ao://worker-reports/batch\"}]"}, id, histClock.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if batch, err := s.ClaimQueuedBatch(ctx, conversation, true, 0, histClock.Add(time.Minute)); err != nil || len(batch) != 40 {
		t.Fatalf("%d %v", len(batch), err)
	}
	if err := s.RequeueUndispatchedTurns(ctx, conversation, ids, "QUOTA_BLOCKED: fake"); err != nil {
		t.Fatal(err)
	}
	queue, err := s.ListQueuedBatch(ctx, conversation, 0)
	if err != nil || len(queue) != 40 {
		t.Fatalf("%d %v", len(queue), err)
	}
	for i, q := range queue {
		if q.TurnID != ids[i] || q.Text != "authored "+ids[i] || q.DeliveryContentJSON == "" {
			t.Fatalf("message lost: %+v", q)
		}
	}
	if _, err := s.ClaimQueuedBatch(ctx, conversation, true, 0, histClock.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, ids[1], "native-bound", histClock.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueUndispatchedTurns(ctx, conversation, ids, "incorrect proof"); err == nil {
		t.Fatal("bound turn was requeued")
	}
	first, err := s.TurnByID(ctx, ids[0])
	if err != nil || first.State != domain.TurnStateRunning {
		t.Fatalf("partial transaction committed: %+v %v", first, err)
	}
}
