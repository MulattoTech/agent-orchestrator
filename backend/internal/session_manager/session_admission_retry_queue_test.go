package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAdmissionQueuedSpawnRetryPreservesSchedulerAndPayload(t *testing.T) {
	t.Run("same_key_keeps_fifo_position", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, true)
		ctx := context.Background()
		now := time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
		f.manager.clock = func() time.Time { return now }
		if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
			t.Fatal(err)
		}
		first := admissionSpawn("blue")
		first.ClientRequestID, first.ClientRequestHash = "first-task", "first-task-hash"
		second := admissionSpawn("blue")
		second.ClientRequestID, second.ClientRequestHash = "second-task", "second-task-hash"
		var queued *SessionQueuedError
		_, _, _, err := f.manager.Spawn(ctx, first)
		if !errors.As(err, &queued) {
			t.Fatalf("first request not queued: %v", err)
		}
		firstID := queued.RequestID
		now = now.Add(time.Second)
		_, _, _, err = f.manager.Spawn(ctx, second)
		if !errors.As(err, &queued) {
			t.Fatalf("second request not queued: %v", err)
		}
		secondID := queued.RequestID
		f.limits.MaxActiveWorkers = 2
		if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
			t.Fatal(err)
		}
		_, _, _, err = f.manager.Spawn(ctx, second)
		if !errors.As(err, &queued) || queued.RequestID != secondID {
			t.Errorf("retry bypassed its queue position: %v", err)
		}
		if live, external := f.rt.counts(); live != 1 || !external {
			t.Errorf("retry launched before scheduler: live=%d external=%t", live, external)
		}
		if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
			t.Fatal(err)
		}
		requests, err := f.manager.ListAdmissionQueue(ctx)
		if err != nil || len(requests) != 2 {
			t.Fatalf("queue=%+v err=%v", requests, err)
		}
		for _, request := range requests {
			switch request.ID {
			case firstID:
				if request.State != "completed" || request.SessionID == "" {
					t.Errorf("oldest command lost first available slot: %+v", request)
				}
			case secondID:
				if request.State != "queued" || request.Generation != 0 {
					t.Errorf("retry advanced its durable claim: %+v", request)
				}
			default:
				t.Fatalf("unexpected command %s", request.ID)
			}
		}
	})

	for _, hashMode := range []string{"changed_hash", "reused_hash", "empty_hash"} {
		t.Run("same_key_cannot_remove_dependency_"+hashMode, func(t *testing.T) {
			f := newAdmissionFixture(t, 2, true)
			ctx := context.Background()
			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "dependent-task", "original-dependent-hash"
			if hashMode == "empty_hash" {
				cfg.ClientRequestHash = ""
			}
			cfg.PRCompletionDependencies = []domain.PRCompletionDependency{{
				SessionID: "upstream-worker", PRURL: "https://github.com/example/repo/pull/7", HeadSHA: strings.Repeat("a", 40),
			}}
			_, _, _, err := f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("dependent request not queued: %v", err)
			}
			before, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(before) != 1 {
				t.Fatalf("queue=%+v err=%v", before, err)
			}
			changed := cfg
			changed.PRCompletionDependencies = nil
			if hashMode == "changed_hash" {
				changed.ClientRequestHash = "changed-without-dependency-hash"
			}
			_, _, _, err = f.manager.Spawn(ctx, changed)
			if !errors.Is(err, ErrClientRequestConflict) {
				t.Errorf("same key accepted changed immutable command: %v", err)
			}
			if live, external := f.rt.counts(); live != 0 || !external {
				t.Errorf("changed retry bypassed dependency: live=%d external=%t", live, external)
			}
			after, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(after) != 1 || after[0].Payload != before[0].Payload ||
				after[0].State != "queued" || after[0].Generation != 0 {
				t.Errorf("retry altered immutable queue command: %+v err=%v", after, err)
			}
		})
	}

	t.Run("delivery_hints_do_not_change_queue_identity", func(t *testing.T) {
		f := newAdmissionFixture(t, 1, true)
		ctx := context.Background()
		if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
			t.Fatal(err)
		}
		cfg := admissionSpawn("blue")
		cfg.ClientRequestID, cfg.ClientRequestHash = "delivery-hints", "delivery-hints-hash"
		cfg.Async, cfg.TaskPreparation = true, "expired-preparation"
		_, _, _, err := f.manager.Spawn(ctx, cfg)
		var queued *SessionQueuedError
		if !errors.As(err, &queued) {
			t.Fatalf("initial request not queued: %v", err)
		}
		id := queued.RequestID
		cfg.Async, cfg.TaskPreparation = false, "replacement-preparation"
		_, _, _, err = f.manager.Spawn(ctx, cfg)
		if !errors.As(err, &queued) || queued.RequestID != id {
			t.Fatalf("delivery hints changed immutable command identity: %v", err)
		}
		if live, external := f.rt.counts(); live != 1 || !external {
			t.Fatalf("delivery retry changed runtimes: live=%d external=%t", live, external)
		}
	})
}
