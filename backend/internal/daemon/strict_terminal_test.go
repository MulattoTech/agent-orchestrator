package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestStrictPolicyRuntimeMessengerCannotBypassGuard(t *testing.T) {
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv("AO_INFERENCE_POLICY", "")
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	if err = store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/synthetic", RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode, domain.HarnessOpenCode} {
		rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: harness, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle}, Metadata: domain.SessionMetadata{RuntimeHandleID: "retained-terminal"}})
		if err != nil {
			t.Fatal(err)
		}
		rt := &captureRuntimeSender{}
		messenger := newSessionMessenger(store, rt, nil)
		if err = messenger.Send(ctx, rec.ID, "implement"); !errors.Is(err, inferenceguard.ErrPolicy) {
			t.Fatalf("%s: %v", harness, err)
		}
		if rt.handle.ID != "" || rt.message != "" {
			t.Fatal("terminal write bypassed strict policy")
		}
	}
}
