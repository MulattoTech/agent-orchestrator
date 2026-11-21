package sessionmanager

import (
	"context"
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

// seedKeyBarrier pauses *after* external request checks but before inserting a
// session seed. Only synthetic fixtures are used; no executable is launched.
type seedKeyBarrier struct {
	*sqlite.Store
	entered chan<- struct{}
	release <-chan struct{}
}

func (s *seedKeyBarrier) CreateClientRequestSession(ctx context.Context, rec domain.SessionRecord) (domain.SessionRecord, bool, error) {
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return domain.SessionRecord{}, false, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return domain.SessionRecord{}, false, ctx.Err()
	}
	return s.Store.CreateClientRequestSession(ctx, rec)
}

func independentSyntheticAdmissionManager(f *admissionFixture, store *sqlite.Store, maxActive int) *Manager {
	limits := f.limits
	limits.MaxActiveWorkers = maxActive
	return New(Deps{
		Runtime: f.rt, Workspace: f.ws, Store: store, Agents: fakeAgents{},
		Lifecycle: lifecycle.New(store, nil), DataDir: f.data, SessionAdmission: limits,
		LookPath: func(string) (string, error) { return os.Executable() },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func waitSyntheticKeyBarrier(t *testing.T, ch <-chan domain.ProjectID) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatal("synthetic concurrent request did not reach SQLite enqueue barrier")
	}
}

func TestAdmissionCrossManagerSameProjectHashConflictIsTyped(t *testing.T) {
	for _, sameHash := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_hash_must_conflict", true: "identical_payload_is_idempotent"}[sameHash], func(t *testing.T) {
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
			other := independentSyntheticAdmissionManager(f, otherStore, 1)
			entered := make(chan domain.ProjectID, 2)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			const key = "synthetic-same-project-queued-key"
			f.manager.store = &dualManagerKeyBarrier{Store: f.store, entered: entered, release: release, key: key}
			other.store = &dualManagerKeyBarrier{Store: otherStore, entered: entered, release: release, key: key}
			results := make(chan error, 2)
			for i, manager := range []*Manager{f.manager, other} {
				cfg := admissionSpawn("blue")
				cfg.ClientRequestID, cfg.ClientRequestHash = key, "hash-1"
				if i == 1 && !sameHash {
					cfg.ClientRequestHash = "hash-2"
				}
				go func(m *Manager, c ports.SpawnConfig) {
					_, _, _, err := m.Spawn(ctx, c)
					results <- err
				}(manager, cfg)
			}
			waitSyntheticKeyBarrier(t, entered)
			waitSyntheticKeyBarrier(t, entered)
			unblock()

			var queued, conflict int
			var id string
			for range 2 {
				select {
				case got := <-results:
					var deferred *SessionQueuedError
					switch {
					case errors.As(got, &deferred):
						if id != "" && id != deferred.RequestID {
							t.Errorf("same project/key created two distinct queue IDs %q vs %q", id, deferred.RequestID)
						}
						id = deferred.RequestID
						queued++
					case errors.Is(got, ErrClientRequestConflict):
						conflict++
					default:
						t.Errorf("unexpected same-project admission outcome: %v", got)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("synthetic same-project requests did not settle")
				}
			}
			commands, err := f.store.ListSessionAdmission(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantQueued, wantConflict := 1, 1
			if sameHash {
				wantQueued, wantConflict = 2, 0
			}
			if queued != wantQueued || conflict != wantConflict || len(commands) != 1 {
				t.Errorf("same-project key: queued=%d conflict=%d durable=%d want queued=%d conflict=%d durable=1",
					queued, conflict, len(commands), wantQueued, wantConflict)
			}
			if len(commands) == 1 && commands[0].ID != id {
				t.Errorf("durable queue ID changed: %q vs %q", commands[0].ID, id)
			}
			if live, sentinel := f.rt.counts(); live != 1 || !sentinel {
				t.Errorf("same-project queue race mutated fake runtimes: live=%d sentinel=%t", live, sentinel)
			}
		})
	}
}

func TestAdmissionCrossManagerQueuedAndSeededKeyCannotCoexist(t *testing.T) {
	t.Run("queued_claim_wins_before_seed", func(t *testing.T) {
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
		seedManager := independentSyntheticAdmissionManager(f, otherStore, 2)
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		seedManager.store = &seedKeyBarrier{Store: otherStore, entered: entered, release: release}
		const key = "synthetic-queued-before-seed-key"
		cfgSeed := admissionSpawn("blue")
		cfgSeed.ClientRequestID, cfgSeed.ClientRequestHash = key, "seed-claim"
		seedResult := make(chan error, 1)
		go func() {
			_, _, _, err := seedManager.Spawn(ctx, cfgSeed)
			seedResult <- err
		}()
		select {
		case <-entered:
		case <-time.After(8 * time.Second):
			t.Fatal("seed did not reach pre-insert barrier")
		}
		cfgQueue := admissionSpawn("green")
		cfgQueue.ClientRequestID, cfgQueue.ClientRequestHash = key, "queued-claim"
		_, _, _, err = f.manager.Spawn(ctx, cfgQueue)
		var deferred *SessionQueuedError
		if !errors.As(err, &deferred) {
			t.Fatalf("expected synthetic queue winner, got %v", err)
		}
		unblock()
		select {
		case err = <-seedResult:
			if !errors.Is(err, ErrClientRequestConflict) {
				t.Errorf("seed bypassed a durable queued key from another Manager: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("seed did not settle after queue won")
		}
		commands, err := f.store.ListSessionAdmission(ctx)
		if err != nil || len(commands) != 1 || commands[0].ID != deferred.RequestID {
			t.Fatalf("queued command changed after seed race: %+v, %v", commands, err)
		}
		if seeded, found, err := f.store.GetSessionByClientRequestID(ctx, key); err != nil || found {
			t.Errorf("queued key also seeded a session: session=%+v found=%t err=%v", seeded, found, err)
		}
		if live, sentinel := f.rt.counts(); live != 1 || !sentinel {
			t.Errorf("queued key launched synthetic runtime: live=%d sentinel=%t", live, sentinel)
		}
	})

	t.Run("session_seed_wins_before_queue_insert", func(t *testing.T) {
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
		seedManager := independentSyntheticAdmissionManager(f, otherStore, 2)
		entered := make(chan domain.ProjectID, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		const key = "synthetic-seed-before-queue-key"
		f.manager.store = &dualManagerKeyBarrier{Store: f.store, entered: entered, release: release, key: key}
		cfgQueue := admissionSpawn("green")
		cfgQueue.ClientRequestID, cfgQueue.ClientRequestHash = key, "queued-claim"
		queueResult := make(chan error, 1)
		go func() {
			_, _, _, err := f.manager.Spawn(ctx, cfgQueue)
			queueResult <- err
		}()
		waitSyntheticKeyBarrier(t, entered)
		cfgSeed := admissionSpawn("blue")
		cfgSeed.ClientRequestID, cfgSeed.ClientRequestHash = key, "seed-claim"
		created, _, _, err := seedManager.Spawn(ctx, cfgSeed)
		if err != nil || created.ID == "" {
			t.Fatalf("independent synthetic seed not created: %s, %v", created.ID, err)
		}
		unblock()
		select {
		case err = <-queueResult:
			if !errors.Is(err, ErrClientRequestConflict) {
				t.Errorf("queue bypassed a durable seeded key from another Manager: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("queued request did not settle after seed")
		}
		commands, err := f.store.ListSessionAdmission(ctx)
		if err != nil || len(commands) != 0 {
			t.Errorf("queue followed existing seeded key: %+v err=%v", commands, err)
		}
		seeded, found, err := f.store.GetSessionByClientRequestID(ctx, key)
		if err != nil || !found || seeded.ID != created.ID || !seeded.ClientRequestCommitted {
			t.Errorf("seeded client receipt missing or changed: found=%t session=%+v err=%v", found, seeded, err)
		}
		if live, sentinel := f.rt.counts(); live != 2 || !sentinel {
			t.Errorf("seed-winning race did not preserve fake runtimes: live=%d sentinel=%t", live, sentinel)
		}
	})
}
