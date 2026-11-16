package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestInferenceRefusalPreservesFortyQueuedMessages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.conv.mu.Lock()
	h.conv.sendErr = &inferenceguard.Refusal{Code: inferenceguard.ErrQuota, Reason: "synthetic exhausted account"}
	h.conv.mu.Unlock()
	var ids []string
	for i := 0; i < 40; i++ {
		msg := ports.ChatUserMessage{Text: fmt.Sprintf("authored task %02d", i), ClientMessageID: fmt.Sprintf("client-%02d", i),
			Content: []ports.ChatContent{{Type: "text", Internal: true, URI: "ao://worker-reports/fixture", Text: "durable structured report"}}}
		turn, err := h.svc.Send(ctx, testSession, msg)
		if !errors.Is(err, inferenceguard.ErrQuota) || turn.State != domain.TurnStateQueued {
			t.Fatalf("turn=%+v error=%v", turn, err)
		}
		ids = append(ids, turn.ID)
		h.advance(time.Second)
	}
	if got := h.conv.sentMessages(); len(got) != 0 {
		t.Fatalf("provider writes=%d", len(got))
	}
	queued, err := h.st.ListQueuedBatch(ctx, h.ctrl.ConversationID(), 0)
	if err != nil || len(queued) != 40 {
		t.Fatalf("queue=%d error=%v", len(queued), err)
	}
	for i, q := range queued {
		if q.TurnID != ids[i] || q.Text != fmt.Sprintf("authored task %02d", i) {
			t.Fatalf("queue order/content changed: %+v", q)
		}
		var blocks []ports.ChatContent
		if err := json.Unmarshal([]byte(q.DeliveryContentJSON), &blocks); err != nil || len(blocks) != 1 || blocks[0].URI != "ao://worker-reports/fixture" || blocks[0].Text != "durable structured report" {
			t.Fatalf("lost delivery: %s %v", q.DeliveryContentJSON, err)
		}
	}
	h.conv.mu.Lock()
	h.conv.sendErr = nil
	h.conv.mu.Unlock()
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if got := h.conv.sentMessages(); len(got) != 1 || got[0].Text != "authored task 00\n\ndurable structured report" {
		t.Fatalf("resume delivered %+v", got)
	}
	if row, err := h.st.TurnByID(ctx, ids[0]); err != nil || row.ProviderTurnID == "" {
		t.Fatalf("missing native binding: %+v %v", row, err)
	}
}
func TestUncertainDispatchNeverRequeued(t *testing.T) {
	h := newHarness(t)
	h.conv.mu.Lock()
	h.conv.sendErr = fmt.Errorf("%w: synthetic partial write", inferenceguard.ErrUncertain)
	h.conv.mu.Unlock()
	turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{Text: "work with uncertain delivery", ClientMessageID: "uncertain"})
	if !errors.Is(err, inferenceguard.ErrUncertain) || turn.State != domain.TurnStateFailed {
		t.Fatalf("%+v %v", turn, err)
	}
	if queued, err := h.st.ListQueuedBatch(context.Background(), h.ctrl.ConversationID(), 0); !errors.Is(err, domain.ErrNoQueuedTurn) || len(queued) != 0 {
		t.Fatalf("uncertain work requeued: %+v %v", queued, err)
	}
}
func strictServicePolicy(t *testing.T, h *harness) *inferenceguard.Gate {
	t.Helper()
	p := inferenceguard.Policy{Version: 1, AccountKey: "fake-account", AuthorizationEpoch: "fake-epoch",
		AuthorityPath: filepath.Join(t.TempDir(), "authority.db"), EmergencyStop: true, MaximumEffort: "medium",
		MaximumInference: 2, MaximumPerProject: 2, MaximumReviewers: 1, TurnBudget: 5, ReserveTurns: 1, Roles: map[string]inferenceguard.RolePolicy{}}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		p.Roles[role] = inferenceguard.RolePolicy{DefaultModel: "fake-model", Models: []string{"fake-model"}}
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(inferenceguard.PolicyEnv, path)
	g, err := inferenceguard.ForDataDir(h.rendersDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}
func TestStrictQuotaSweeperNeverSwitchesAccountOrRetries(t *testing.T) {
	hh := newHandoffHarness(t, domain.HarnessCodex)
	source := failUsageLimitedTurn(t, hh.harness, usageLimitMessage(domain.HarnessCodex, true, hh.now()))
	ctx := context.Background()
	if err := hh.svc.SetTurnAutoResume(ctx, testSession, source.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = strictServicePolicy(t, hh.harness)
	hh.advance(2 * time.Hour)
	before := hh.conv.sentMessages()
	for i := 0; i < 10; i++ {
		if err := hh.svc.ResumeUsageLimitedTurns(ctx); err != nil {
			t.Fatal(err)
		}
		if err := hh.svc.HandoffUsageLimitedTurns(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, hh.conv.sentMessages()) || len(hh.mover.moves()) != 0 || hh.accounts.calls != 0 {
		t.Fatalf("quota sweeper retried or switched accounts")
	}
	turn, err := hh.st.TurnByID(ctx, source.ID)
	if err != nil || turn.UsageLimit == nil || !turn.UsageLimit.AutoResume {
		t.Fatalf("lost durable consent: %+v %v", turn, err)
	}
}
