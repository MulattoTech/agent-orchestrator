package sessionmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// The matching running queue may insert its own seed while a second Manager
// must still be unable to create a conflicting command or launch a runtime.
// All runtimes, workspaces and databases in this test are synthetic.
func TestAdmissionCrossManagerMatchingDispatcherOwnsSeed(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
		t.Fatal(err)
	}
	cfg := admissionSpawn("blue")
	cfg.ClientRequestID = "synthetic-matching-dispatch-client-key"
	cfg.ClientRequestHash = "matched-owner-hash"
	_, _, _, err := f.manager.Spawn(ctx, cfg)
	var queued *SessionQueuedError
	if !errors.As(err, &queued) {
		t.Fatalf("expected queued admission: %v", err)
	}
	if queued.RequestID == "" {
		t.Fatal("missing durable queued request ID")
	}
	if err := f.manager.UpdateAdmissionLimits(ctx, domain.SessionAdmissionLimits{
		RegisteredSlots: 40, MaxActiveWorkers: 2, DefaultProjectWorkers: 40, QueueEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	otherStore, err := sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })
	otherManager := independentSyntheticAdmissionManager(f, otherStore, 2)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.manager.store = &seedKeyBarrier{Store: f.store, entered: entered, release: release}
	dispatched := make(chan error, 1)
	go func() { dispatched <- f.manager.DispatchAdmissionQueue(ctx, 1) }()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("running owner did not reach synthetic pre-seed barrier")
	}
	// This second Manager owns no launch. A mismatched hash must conflict while
	// the first Manager holds the durable running command claim.
	conflicting := cfg
	conflicting.ClientRequestHash = "wrong-payload-hash"
	if _, _, _, err := otherManager.Spawn(ctx, conflicting); !errors.Is(err, ErrClientRequestConflict) {
		t.Errorf("different hash acquired running queue key: %v", err)
	}
	// The exact same request is still queued/in-flight, not a new session.
	if _, _, _, err := otherManager.Spawn(ctx, cfg); !errors.As(err, &queued) {
		t.Errorf("matching external retry bypassed running queue: %v", err)
	}
	unblock()
	select {
	case err := <-dispatched:
		if err != nil {
			t.Fatalf("matched dispatcher did not finish: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("synthetic dispatcher did not settle")
	}
	receipt, found, err := f.store.GetSessionByClientRequestID(ctx, cfg.ClientRequestID)
	if err != nil || !found || !receipt.ClientRequestCommitted || receipt.ProjectID != cfg.ProjectID {
		t.Fatalf("durable session receipt not committed: found=%t err=%v receipt=%+v", found, err, receipt)
	}
	commands, err := f.store.ListSessionAdmission(ctx)
	if err != nil || len(commands) != 1 || commands[0].State != "completed" || commands[0].SessionID != receipt.ID {
		t.Errorf("matching command did not complete once: %+v err=%v", commands, err)
	}
	if got, _, _, err := otherManager.Spawn(ctx, cfg); err != nil || got.ID != receipt.ID {
		t.Errorf("exact completed receipt did not replay: id=%s want=%s err=%v", got.ID, receipt.ID, err)
	}
	if _, _, _, err := otherManager.Spawn(ctx, conflicting); !errors.Is(err, ErrClientRequestConflict) {
		t.Errorf("completed request changed hash ownership: %v", err)
	}
	if live, sentinel := f.rt.counts(); live != 2 || !sentinel {
		t.Errorf("cross-Manager dispatch duplicated fake runtime: live=%d sentinel=%t", live, sentinel)
	}
}
