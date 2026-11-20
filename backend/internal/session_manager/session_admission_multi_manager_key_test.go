package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// dualManagerKeyBarrier synchronizes two independent Managers immediately
// before their distinct SQLite Store objects enqueue the same global client
// key in different projects. The fake runtime never starts an executable.
type dualManagerKeyBarrier struct {
	*sqlite.Store
	entered chan<- domain.ProjectID
	release <-chan struct{}
	key     string
}

func (s *dualManagerKeyBarrier) EnqueueSessionAdmission(ctx context.Context, rec domain.SessionAdmissionRequest) error {
	var payload ports.SpawnConfig
	if rec.Operation == "spawn" {
		if err := json.Unmarshal([]byte(rec.Payload), &payload); err != nil {
			return err
		}
	}
	if payload.ClientRequestID == s.key {
		select {
		case s.entered <- rec.ProjectID:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.EnqueueSessionAdmission(ctx, rec)
}

func TestAdmissionCrossManagerClientKeyIsAtomicAcrossQueuedProjects(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
		t.Fatal(err)
	}
	otherStore, err := sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })

	const key = "synthetic-cross-manager-race-key"
	entered := make(chan domain.ProjectID, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.manager.store = &dualManagerKeyBarrier{
		Store: f.store, entered: entered, release: release, key: key,
	}
	otherManager := New(Deps{
		Runtime: f.rt, Workspace: f.ws, Store: &dualManagerKeyBarrier{
			Store: otherStore, entered: entered, release: release, key: key,
		},
		Agents: fakeAgents{}, Lifecycle: lifecycle.New(otherStore, nil),
		DataDir: f.data, SessionAdmission: f.limits,
		LookPath: func(string) (string, error) { return os.Executable() },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	type outcome struct {
		project domain.ProjectID
		err     error
	}
	results := make(chan outcome, 2)
	for _, operation := range []struct {
		manager *Manager
		project domain.ProjectID
		hash    string
	}{
		{f.manager, "blue", "blue-payload"},
		{otherManager, "green", "green-payload"},
	} {
		go func(op struct {
			manager *Manager
			project domain.ProjectID
			hash    string
		}) {
			cfg := admissionSpawn(op.project)
			cfg.ClientRequestID, cfg.ClientRequestHash = key, op.hash
			_, _, _, err := op.manager.Spawn(ctx, cfg)
			results <- outcome{project: op.project, err: err}
		}(operation)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(6 * time.Second):
			t.Fatal("both independently owned requests did not reach the durable enqueue barrier")
		}
	}
	// Both Managers passed the non-atomic admission key precheck. A globally
	// unique client key must still be decided by the single SQLite writer.
	unblock()

	var queued, conflict int
	for range 2 {
		select {
		case got := <-results:
			var deferred *SessionQueuedError
			switch {
			case errors.As(got.err, &deferred):
				queued++
			case errors.Is(got.err, ErrClientRequestConflict):
				conflict++
			default:
				t.Errorf("unexpected %s admission outcome: %v", got.project, got.err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("concurrent stores did not settle")
		}
	}
	requests, err := f.store.ListSessionAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || conflict != 1 || len(requests) != 1 {
		t.Fatalf("global client key race: queued=%d conflict=%d durable=%d; expected one immutable command",
			queued, conflict, len(requests))
	}
	if requests[0].ProjectID != "blue" && requests[0].ProjectID != "green" {
		t.Fatalf("unexpected queued project %q", requests[0].ProjectID)
	}
	if live, sentinel := f.rt.counts(); live != 1 || !sentinel {
		t.Fatalf("synthetic runtime mutated during rejected duplicate: live=%d sentinel=%v", live, sentinel)
	}
}
