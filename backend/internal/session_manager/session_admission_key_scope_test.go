package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestAdmissionClientRequestKeyIsGlobalBeforeReceipt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		restart   bool
		expand    bool
		disable   bool
		hash      string
		retryHash string
	}{
		{name: "queued_full_changed_hash", hash: "blue-hash", retryHash: "green-hash"},
		{name: "queued_expanded_reused_hash", expand: true, hash: "same-hash", retryHash: "same-hash"},
		{name: "cancelled_expanded", cancel: true, expand: true, hash: "blue-hash", retryHash: "green-hash"},
		{name: "cancelled_reopened", cancel: true, restart: true, expand: true, hash: "blue-hash", retryHash: "green-hash"},
		{name: "queued_reopened_disabled", restart: true, expand: true, disable: true, hash: "blue-hash", retryHash: "green-hash"},
		{name: "cancelled_empty_hash", cancel: true, expand: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
			if err != nil {
				t.Fatal(err)
			}
			dirtyPath := filepath.Join(occupied.Metadata.WorkspacePath, "owner-dirty.txt")
			dirty := []byte("synthetic owner content survives client-key conflicts")
			if err := os.WriteFile(dirtyPath, dirty, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "global-task-key", tc.hash
			_, _, _, err = f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("initial request was not queued: %v", err)
			}
			if tc.cancel {
				if ok, err := f.manager.CancelAdmissionRequest(ctx, queued.RequestID); err != nil || !ok {
					t.Fatalf("cancel: applied=%t err=%v", ok, err)
				}
			}
			before, found, err := f.store.GetSessionAdmission(ctx, queued.RequestID)
			if err != nil || !found {
				t.Fatalf("read original: found=%t err=%v", found, err)
			}
			if tc.expand {
				f.limits.MaxActiveWorkers = 3
			}
			f.limits.QueueEnabled = !tc.disable
			if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
				t.Fatal(err)
			}
			if tc.restart {
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
			}

			// The API fingerprint changes with the project. Direct callers
			// can also omit or reuse a hash; neither creates a new key scope.
			retry := cfg
			retry.ProjectID, retry.ClientRequestHash = "green", tc.retryHash
			created, _, _, retryErr := f.manager.Spawn(ctx, retry)
			if !errors.Is(retryErr, ErrClientRequestConflict) {
				t.Errorf("cross-project key reuse must conflict: session=%s err=%v", created.ID, retryErr)
			}
			after, err := f.store.ListSessionAdmission(ctx)
			if err != nil || len(after) != 1 || !reflect.DeepEqual(after[0], before) {
				t.Errorf("retry changed immutable command/history: before=%+v after=%+v err=%v", before, after, err)
			}
			if rec, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID); err != nil || found {
				t.Errorf("conflicting retry acquired a session: id=%s found=%t err=%v", rec.ID, found, err)
			}
			if live, external := f.rt.counts(); live != 1 || !external {
				t.Errorf("conflicting retry changed fake runtimes: live=%d external=%t", live, external)
			}
			mtechAssertDirtyContent(t, dirtyPath, dirty)
			if f.ws.forceCalls.Load() != 0 {
				t.Error("conflicting retry forced workspace removal")
			}
		})
	}
}

func TestAdmissionGeneratedQueueKeyRemainsInternal(t *testing.T) {
	t.Run("omitted_keys_are_distinct_submissions", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, true)
		ctx := context.Background()
		if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
			t.Fatal(err)
		}
		cfg := admissionSpawn("blue")
		ids := make([]string, 0, 2)
		for range 2 {
			_, _, _, err := f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) || queued.RequestID == "" {
				t.Fatalf("keyless request was not queued: %v", err)
			}
			ids = append(ids, queued.RequestID)
			request, found, err := f.store.GetSessionAdmission(ctx, queued.RequestID)
			if err != nil || !found {
				t.Fatalf("read generated request: found=%t err=%v", found, err)
			}
			var stored ports.SpawnConfig
			if err := json.Unmarshal([]byte(request.Payload), &stored); err != nil {
				t.Fatal(err)
			}
			if stored.ClientRequestID != request.ID || stored.ClientRequestHash == "" {
				t.Fatalf("missing internal replay identity: %+v", stored)
			}
		}
		if ids[0] == ids[1] {
			t.Fatal("keyless submissions were incorrectly deduplicated")
		}
		if ok, err := f.manager.CancelAdmissionRequest(ctx, ids[0]); err != nil || !ok {
			t.Fatalf("cancel generated handle: applied=%t err=%v", ok, err)
		}
		second, found, err := f.store.GetSessionAdmission(ctx, ids[1])
		if err != nil || !found || second.State != "queued" {
			t.Fatalf("cancellation changed another keyless submission: %+v found=%t err=%v", second, found, err)
		}
		if live, external := f.rt.counts(); live != 1 || !external {
			t.Fatalf("keyless queue mutated runtimes: live=%d external=%t", live, external)
		}
	})

	t.Run("lost_ack_replays_generated_receipt_after_restart", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, true)
		ctx := context.Background()
		if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue"))
		var queued *SessionQueuedError
		if !errors.As(err, &queued) {
			t.Fatalf("keyless request was not queued: %v", err)
		}
		f.limits.MaxActiveWorkers = 2
		if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
			t.Fatal(err)
		}
		claimed, found, err := f.store.ClaimSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC())
		if err != nil || !found || claimed.ID != queued.RequestID {
			t.Fatalf("claim: %+v found=%t err=%v", claimed, found, err)
		}
		var cfg ports.SpawnConfig
		if err := json.Unmarshal([]byte(claimed.Payload), &cfg); err != nil {
			t.Fatal(err)
		}
		commandCtx := context.WithValue(ctx, admissionQueueContextKey{}, claimed.ID)
		created, _, _, err := f.manager.Spawn(commandCtx, cfg)
		if err != nil || created.ID == "" {
			t.Fatalf("claimed synthetic spawn: id=%s err=%v", created.ID, err)
		}
		receipt, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID)
		if err != nil || !found || !receipt.ClientRequestCommitted || receipt.ID != created.ID {
			t.Fatalf("generated receipt missing: %+v found=%t err=%v", receipt, found, err)
		}

		// Simulate a daemon crash after session commit but before the durable
		// queue acknowledgement. Only fake runtimes survive the store reopen.
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
		if err := f.manager.DispatchAdmissionQueue(ctx, 2); err != nil {
			t.Fatal(err)
		}
		completed, found, err := f.store.GetSessionAdmission(ctx, claimed.ID)
		if err != nil || !found || completed.State != "completed" || completed.SessionID != created.ID ||
			completed.Payload != claimed.Payload || completed.Generation <= claimed.Generation {
			t.Fatalf("generated command did not replay durably: %+v found=%t err=%v", completed, found, err)
		}
		if live, external := f.rt.counts(); live != 2 || !external {
			t.Fatalf("generated recovery duplicated runtime: live=%d external=%t", live, external)
		}
	})
}

func TestAdmissionCrossProjectInFlightKeyDoesNotDefer(t *testing.T) {
	for _, sameHash := range []bool{false, true} {
		name := "changed_hash_conflicts"
		want := ErrClientRequestConflict
		if sameHash {
			name, want = "reused_hash_stays_incomplete", ErrClientRequestIncomplete
		}
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			f.manager.harnessUseGate = &admissionPausedHarnessGate{entered: entered, release: release}
			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "inflight-global-key", "original-hash"
			finished := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				defer close(finished)
				_, _, _, err := f.manager.Spawn(ctx, cfg)
				result <- err
			}()
			t.Cleanup(func() {
				unblock()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("synthetic original did not finish")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("synthetic original did not reach pre-seed barrier")
			}
			retry := cfg
			retry.ProjectID = "green"
			if !sameHash {
				retry.ClientRequestHash = "other-project-hash"
			}
			if _, _, _, err := f.manager.Spawn(ctx, retry); !errors.Is(err, want) {
				t.Errorf("cross-project retry escaped in-flight key: got=%v want=%v", err, want)
			}
			requests, err := f.store.ListSessionAdmission(ctx)
			if err != nil || len(requests) != 0 {
				t.Errorf("in-flight retry gained a cancellable command: %+v err=%v", requests, err)
			}
			unblock()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("synthetic original did not finish")
			}
			rec, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID)
			if err != nil || !found || !rec.ClientRequestCommitted || rec.ProjectID != cfg.ProjectID {
				t.Fatalf("original lost its receipt: %+v found=%t err=%v", rec, found, err)
			}
			if live, external := f.rt.counts(); live != 1 || !external {
				t.Fatalf("concurrent retry changed runtimes: live=%d external=%t", live, external)
			}
		})
	}
}
