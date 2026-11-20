package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestAdmissionCancelledSpawnRetryKeepsCommandTerminal(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same_daemon"
		if restart {
			name = "reopened_sqlite"
		}
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
			if err != nil {
				t.Fatal(err)
			}
			dirtyPath := filepath.Join(occupied.Metadata.WorkspacePath, "owner-dirty.txt")
			dirty := []byte("cancelled admission must preserve existing work")
			if err := os.WriteFile(dirtyPath, dirty, 0600); err != nil {
				t.Fatal(err)
			}

			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "cancelled-task", "cancelled-task-hash"
			_, _, _, err = f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("expected durable queue before cancellation: %v", err)
			}
			requestID := queued.RequestID
			if ok, err := f.manager.CancelAdmissionRequest(ctx, requestID); err != nil || !ok {
				t.Fatalf("cancel request: applied=%t err=%v", ok, err)
			}
			before, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(before) != 1 || before[0].State != "cancelled" {
				t.Fatalf("cancelled request: %+v err=%v", before, err)
			}

			// More capacity authorizes a new command, not revival of the
			// immutable command that the owner already cancelled.
			f.limits.MaxActiveWorkers = 3
			if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
				t.Fatal(err)
			}
			if restart {
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				f.store, err = sqlite.Open(f.data)
				if err != nil {
					t.Fatal(err)
				}
				f.rebuild()
				if err := f.manager.LoadAdmissionLimits(ctx); err != nil {
					t.Fatal(err)
				}
				if err := f.store.RecoverSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}

			retried, _, _, retryErr := f.manager.Spawn(ctx, cfg)
			if retryErr == nil || errors.Is(retryErr, ErrSessionQueued) {
				t.Errorf("cancelled command retry must remain terminal: session=%s err=%v", retried.ID, retryErr)
			}
			if _, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID); err != nil || found {
				t.Errorf("cancelled command acquired a session: found=%t err=%v", found, err)
			}
			if live, external := f.rt.counts(); live != 1 || !external {
				t.Errorf("cancelled command changed fake runtimes: live=%d external=%t", live, external)
			}
			if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
				t.Fatal(err)
			}
			after, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(after) != 1 || after[0].State != "cancelled" ||
				after[0].Generation != before[0].Generation || after[0].SessionID != "" {
				t.Fatalf("cancelled command changed: %+v err=%v", after, err)
			}
			mtechAssertDirtyContent(t, dirtyPath, dirty)
			if f.ws.forceCalls.Load() != 0 {
				t.Fatal("cancelled command forced workspace removal")
			}

			// A different key is explicit fresh intent and remains admissible.
			fresh := cfg
			fresh.ClientRequestID, fresh.ClientRequestHash = "fresh-task", "fresh-task-hash"
			created, _, _, err := f.manager.Spawn(ctx, fresh)
			if err != nil || created.ID == "" {
				t.Fatalf("fresh command refused: session=%s err=%v", created.ID, err)
			}
			if live, external := f.rt.counts(); live != 2 || !external {
				t.Errorf("fresh command count: live=%d external=%t", live, external)
			}
		})
	}
}
