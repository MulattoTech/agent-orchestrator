package sessionmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// admissionPausedHarnessGate stops a synthetic Spawn after it reserves a worker
// slot but before it writes a session seed. It never looks up or starts a binary.
type admissionPausedHarnessGate struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (g *admissionPausedHarnessGate) TryBeginHarnessUse(domain.AgentHarness) (func(), bool) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return func() {}, true
}

func TestAdmissionConcurrentSpawnRetryDoesNotCreateCancellableCommand(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	f.manager.harnessUseGate = &admissionPausedHarnessGate{entered: entered, release: release}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	type outcome struct {
		session domain.SessionRecord
		err     error
	}
	result := make(chan outcome, 1)
	finished := make(chan struct{})
	cfg := admissionSpawn("blue")
	cfg.ClientRequestID, cfg.ClientRequestHash = "concurrent-task", "concurrent-task-hash"
	go func() {
		defer close(finished)
		rec, _, _, err := f.manager.Spawn(ctx, cfg)
		result <- outcome{session: rec, err: err}
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("synthetic spawn did not finish after releasing fixture")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic spawn did not reach the pre-seed barrier")
	}
	if _, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID); err != nil || found {
		t.Fatalf("fixture must pause before seed: found=%t err=%v", found, err)
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatalf("fixture reached runtime before seed barrier: %d", live)
	}

	_, _, _, retryErr := f.manager.Spawn(ctx, cfg)
	if !errors.Is(retryErr, ErrClientRequestIncomplete) {
		t.Errorf("concurrent retry must report the in-flight command: %v", retryErr)
	}
	var queued *SessionQueuedError
	if errors.As(retryErr, &queued) {
		// Record the broken boundary on the baseline: the retry exposes a
		// successful cancel while the original same-key command still owns
		// the reservation and will continue to launch.
		cancelled, err := f.manager.CancelAdmissionRequest(ctx, queued.RequestID)
		t.Errorf("in-flight command gained a cancellable duplicate: cancelled=%t err=%v", cancelled, err)
	}
	requests, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(requests) != 0 {
		t.Errorf("concurrent retry created a second durable command: %+v err=%v", requests, err)
	}

	unblock()
	var started outcome
	select {
	case started = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic spawn did not finish")
	}
	if started.err != nil || started.session.ID == "" {
		t.Fatalf("original command failed: session=%s err=%v", started.session.ID, started.err)
	}
	replayed, _, _, err := f.manager.Spawn(ctx, cfg)
	if err != nil || replayed.ID != started.session.ID {
		t.Fatalf("committed result did not replay: session=%s err=%v", replayed.ID, err)
	}
	if live, external := f.rt.counts(); live != 1 || !external {
		t.Fatalf("same-key commands changed runtime ownership: live=%d external=%t", live, external)
	}
}
