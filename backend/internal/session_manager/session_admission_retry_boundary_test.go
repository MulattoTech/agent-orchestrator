package sessionmanager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type admissionBoundaryStore struct {
	*sqlite.Store
	afterEnqueue func(context.Context, domain.SessionAdmissionRequest) error
	afterCommit  func(context.Context, domain.SessionID) error
	beforeLookup func(context.Context, string) error
}

func (s *admissionBoundaryStore) EnqueueSessionAdmission(ctx context.Context, request domain.SessionAdmissionRequest) error {
	if err := s.Store.EnqueueSessionAdmission(ctx, request); err != nil {
		return err
	}
	if s.afterEnqueue != nil {
		return s.afterEnqueue(ctx, request)
	}
	return nil
}

func (s *admissionBoundaryStore) CommitClientRequestSession(ctx context.Context, id domain.SessionID) error {
	if err := s.Store.CommitClientRequestSession(ctx, id); err != nil {
		return err
	}
	if s.afterCommit != nil {
		return s.afterCommit(ctx, id)
	}
	return nil
}

func (s *admissionBoundaryStore) GetSessionAdmission(ctx context.Context, id string) (domain.SessionAdmissionRequest, bool, error) {
	if s.beforeLookup != nil {
		if err := s.beforeLookup(ctx, id); err != nil {
			return domain.SessionAdmissionRequest{}, false, err
		}
	}
	return s.Store.GetSessionAdmission(ctx, id)
}

type admissionHarnessGateFunc func(domain.AgentHarness) (func(), bool)

func (f admissionHarnessGateFunc) TryBeginHarnessUse(harness domain.AgentHarness) (func(), bool) {
	return f(harness)
}

func TestAdmissionSpawnRetryHonorsDurableStates(t *testing.T) {
	for _, state := range []string{"queued", "running", "blocked", "cancelled", "completed"} {
		t.Run(state, func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
				t.Fatal(err)
			}
			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "state-task", "state-task-hash"
			_, _, _, err := f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("request not queued: %v", err)
			}
			id := queued.RequestID
			if state == "cancelled" {
				if ok, err := f.manager.CancelAdmissionRequest(ctx, id); err != nil || !ok {
					t.Fatalf("cancel: %t %v", ok, err)
				}
			} else if state != "queued" {
				request, claimed, err := f.store.ClaimSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC())
				if err != nil || !claimed || request.ID != id {
					t.Fatalf("claim: %+v %t %v", request, claimed, err)
				}
				if state != "running" {
					request.State = state
					if applied, err := f.store.CompleteSessionAdmission(ctx, request); err != nil || !applied {
						t.Fatalf("record state: %t %v", applied, err)
					}
				}
			}
			before, found, err := f.store.GetSessionAdmission(ctx, id)
			if err != nil || !found || before.State != state {
				t.Fatalf("fixture state: %+v %t %v", before, found, err)
			}
			// Queue disablement prevents future claims, not the identity guard.
			f.limits.MaxActiveWorkers = 2
			f.limits.QueueEnabled = false
			if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = f.manager.Spawn(ctx, cfg)
			if state == "queued" || state == "running" {
				if !errors.As(err, &queued) || queued.RequestID != id {
					t.Errorf("pending command escaped its queue: %v", err)
				}
			} else {
				var terminal *SessionAdmissionStateError
				if !errors.As(err, &terminal) || terminal.RequestID != id || terminal.State != state {
					t.Errorf("terminal command lost its durable state: %v", err)
				}
			}
			after, found, err := f.store.GetSessionAdmission(ctx, id)
			if err != nil || !found || after.State != before.State || after.Generation != before.Generation ||
				after.Owner != before.Owner || after.Payload != before.Payload || after.SessionID != "" {
				t.Errorf("direct retry mutated command: %+v %t %v", after, found, err)
			}
			if live, external := f.rt.counts(); live != 1 || !external {
				t.Errorf("direct retry launched a fake runtime: live=%d external=%t", live, external)
			}
		})
	}
}

func TestAdmissionSpawnRequestGatePreservesReceiptsAndRelease(t *testing.T) {
	t.Run("committed_receipt_replays_while_gate_busy", func(t *testing.T) {
		f := newAdmissionFixture(t, 2, true)
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		var unblockOnce sync.Once
		unblock := func() { unblockOnce.Do(func() { close(release) }) }
		wrapped := &admissionBoundaryStore{Store: f.store}
		wrapped.afterCommit = func(context.Context, domain.SessionID) error {
			close(entered)
			<-release
			return nil
		}
		f.manager.store = wrapped
		cfg := admissionSpawn("blue")
		cfg.ClientRequestID, cfg.ClientRequestHash = "busy-receipt", "busy-receipt-hash"
		result := make(chan error, 1)
		finished := make(chan struct{})
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
				t.Error("synthetic commit did not finish")
			}
		})
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("synthetic commit did not reach barrier")
		}
		receipt, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID)
		if err != nil || !found || !receipt.ClientRequestCommitted {
			t.Fatalf("committed receipt missing: found=%t err=%v", found, err)
		}
		replayed, _, _, err := f.manager.Spawn(ctx, cfg)
		if err != nil || replayed.ID != receipt.ID {
			t.Errorf("busy key hid committed receipt: session=%s err=%v", replayed.ID, err)
		}
		unblock()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if live, external := f.rt.counts(); live != 1 || !external {
			t.Fatalf("receipt replay changed runtimes: live=%d external=%t", live, external)
		}
	})

	t.Run("unrelated_key_remains_concurrent", func(t *testing.T) {
		f := newAdmissionFixture(t, 2, true)
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		var first atomic.Bool
		var unblockOnce sync.Once
		unblock := func() { unblockOnce.Do(func() { close(release) }) }
		f.manager.harnessUseGate = admissionHarnessGateFunc(func(domain.AgentHarness) (func(), bool) {
			if first.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			return func() {}, true
		})
		cfg := admissionSpawn("blue")
		cfg.ClientRequestID, cfg.ClientRequestHash = "first-key", "first-key-hash"
		result := make(chan error, 1)
		finished := make(chan struct{})
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
				t.Error("synthetic first key did not finish")
			}
		})
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("synthetic first key did not reach barrier")
		}
		other := admissionSpawn("green")
		other.ClientRequestID, other.ClientRequestHash = "unrelated-key", "unrelated-key-hash"
		if _, _, _, err := f.manager.Spawn(ctx, other); err != nil {
			t.Fatalf("unrelated key serialized behind first key: %v", err)
		}
		unblock()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if live, external := f.rt.counts(); live != 2 || !external {
			t.Fatalf("independent keys: live=%d external=%t", live, external)
		}
	})

	t.Run("failed_fork_validation_releases_key_without_serializing_callbacks", func(t *testing.T) {
		f := newAdmissionFixture(t, 2, true)
		ctx := context.Background()
		cfg := admissionSpawn("blue")
		cfg.ClientRequestID, cfg.ClientRequestHash = "invalid-task", "invalid-task-hash"
		cfg.Branch = "unsupported-scratch-branch"
		cfg.Fork = &ports.SpawnFork{Handoff: func(context.Context, ports.ForkTarget) (string, error) {
			t.Error("fork callback ran before branch validation")
			return "", nil
		}}
		if _, _, _, err := f.manager.Spawn(ctx, cfg); !errors.Is(err, ErrScratchBranchUnsupported) {
			t.Fatalf("entry gate changed ordinary Fork validation: %v", err)
		}
		cfg.Branch, cfg.Fork, cfg.ClientRequestHash = "", nil, "valid-task-hash"
		if _, _, _, err := f.manager.Spawn(ctx, cfg); err != nil {
			t.Fatalf("failed Spawn retained its key gate: %v", err)
		}
	})
}

func TestAdmissionDispatchRacesReplayCommittedReceipt(t *testing.T) {
	for _, boundary := range []string{"enqueue_before_return", "terminal_lookup_after_receipt_miss"} {
		t.Run(boundary, func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
				t.Fatal(err)
			}
			cfg := admissionSpawn("blue")
			cfg.ClientRequestID, cfg.ClientRequestHash = "dispatch-race", "dispatch-race-hash"
			wrapped := &admissionBoundaryStore{Store: f.store}
			f.manager.store = wrapped
			dispatch := func() error {
				f.limits.MaxActiveWorkers = 2
				if err := f.manager.UpdateAdmissionLimits(ctx, f.limits); err != nil {
					return err
				}
				return f.manager.DispatchAdmissionQueue(ctx, 1)
			}
			if boundary == "enqueue_before_return" {
				wrapped.afterEnqueue = func(context.Context, domain.SessionAdmissionRequest) error { return dispatch() }
			}
			_, _, _, err := f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatalf("initial request not durably queued: %v", err)
			}
			if boundary == "terminal_lookup_after_receipt_miss" {
				// External Spawn reads no session first. Its queue lookup then
				// synchronously runs the real dispatcher while its key gate is held.
				var once sync.Once
				var dispatchErr error
				wrapped.beforeLookup = func(context.Context, string) error {
					once.Do(func() { dispatchErr = dispatch() })
					return dispatchErr
				}
			}
			replayed, _, _, err := f.manager.Spawn(ctx, cfg)
			if err != nil || replayed.ID == "" {
				t.Fatalf("dispatch race lost committed result: session=%s err=%v", replayed.ID, err)
			}
			requests, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(requests) != 1 || requests[0].State != "completed" ||
				requests[0].SessionID != replayed.ID {
				t.Fatalf("claimed dispatcher did not complete once: %+v err=%v", requests, err)
			}
			if live, external := f.rt.counts(); live != 2 || !external {
				t.Fatalf("dispatch race duplicated runtime: live=%d external=%t", live, external)
			}
		})
	}
}
