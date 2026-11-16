package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type admissionFixture struct {
	manager *Manager
	store   *sqlite.Store
	rt      *mtechFleetRuntime
	ws      *mtechFleetWorkspace
	data    string
	limits  domain.SessionAdmissionLimits
}

func newAdmissionFixture(t *testing.T, limit int, queue bool) *admissionFixture {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	store, err := sqlite.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	f := &admissionFixture{store: store, data: data,
		rt:     &mtechFleetRuntime{live: map[string]domain.SessionID{"unowned-sentinel": "external"}},
		ws:     &mtechFleetWorkspace{root: filepath.Join(root, "workspaces"), owned: map[string]ports.WorkspaceInfo{}},
		limits: domain.SessionAdmissionLimits{RegisteredSlots: 40, MaxActiveWorkers: limit, DefaultProjectWorkers: 40, QueueEnabled: queue}}
	t.Cleanup(func() { _ = f.store.Close() })
	for _, id := range []domain.ProjectID{"red", "blue", "green"} {
		project := domain.ProjectRecord{ID: string(id), Kind: domain.ProjectKindScratch, Path: filepath.Join(root, string(id)), RegisteredAt: time.Now().UTC(), Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessAider}}}
		if err := os.MkdirAll(project.Path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertProject(context.Background(), project); err != nil {
			t.Fatal(err)
		}
	}
	f.rebuild()
	return f
}
func (f *admissionFixture) rebuild() {
	f.manager = New(Deps{Runtime: f.rt, Agents: fakeAgents{}, Workspace: f.ws, Store: f.store, Lifecycle: lifecycle.New(f.store, nil),
		DataDir: f.data, SessionAdmission: f.limits, LookPath: func(string) (string, error) { return os.Executable() }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}
func admissionSpawn(project domain.ProjectID) ports.SpawnConfig {
	return ports.SpawnConfig{ProjectID: project, Kind: domain.KindWorker, Harness: domain.HarnessAider, RequestedMode: domain.SessionModeTUI, Prompt: "SYNTHETIC admission fixture; no executable is run", AgentConfigResolved: true}
}

func TestAdmissionSyntheticFleetStages(t *testing.T) {
	for _, slots := range []int{2, 4, 8, 16, 20, 30, 40} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			f := newAdmissionFixture(t, slots, false)
			ctx := context.Background()
			started := time.Now()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			start := make(chan struct{})
			results := make(chan error, 64)
			var wg sync.WaitGroup
			for i := range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, _, _, err := f.manager.Spawn(ctx, admissionSpawn([]domain.ProjectID{"red", "blue", "green"}[i%3]))
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				} else if !errors.Is(err, ErrWorkerCapacity) {
					t.Fatal(err)
				}
			}
			if success == 0 || success > slots {
				t.Fatalf("admission successful=%d cap=%d", success, slots)
			}
			// A reservation can conservatively overlap its newly inserted row
			// for a few instructions; settled capacity must be usable.
			for success < slots {
				_, _, _, err := f.manager.Spawn(ctx, admissionSpawn("green"))
				if err != nil {
					t.Fatal(err)
				}
				success++
			}
			snapshot, err := f.manager.AdmissionSnapshot(ctx)
			if err != nil || snapshot.ActiveWorkerSessions != slots || snapshot.ReservedWorkers != 0 {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
			if live, external := f.rt.counts(); live != slots || !external {
				t.Fatalf("fake processes=%d external=%t", live, external)
			}
			// A fresh Manager and reopened SQLite database must count surviving
			// controllers before allowing another worker.
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = sqlite.Open(f.data)
			if err != nil {
				t.Fatal(err)
			}
			f.rebuild()
			if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); !errors.Is(err, ErrWorkerCapacity) {
				t.Fatalf("restart exceeded cap: %v", err)
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			t.Logf("SYNTHETIC_ADMISSION slots=%d projects=3 contenders=64 active=%d elapsed_ms=%d heap_delta=%d goroutines=%d provider_calls=0", slots, snapshot.ActiveWorkerSessions, time.Since(started).Milliseconds(), int64(after.HeapAlloc)-int64(before.HeapAlloc), runtime.NumGoroutine())
		})
	}
}

func TestAdmissionQueueFortyTasksQuotaRestartAndExactlyOnce(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	f.manager.clock = func() time.Time { return now }
	occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		cfg := admissionSpawn([]domain.ProjectID{"red", "blue", "green"}[i%3])
		cfg.ClientRequestID = fmt.Sprintf("requested-%02d", i)
		cfg.ClientRequestHash = fmt.Sprintf("hash-%02d", i)
		_, _, _, err := f.manager.Spawn(ctx, cfg)
		var queued *SessionQueuedError
		if !errors.As(err, &queued) {
			t.Fatalf("request %d not queued: %v", i, err)
		}
		_, _, _, err = f.manager.Spawn(ctx, cfg)
		if !errors.As(err, &queued) {
			t.Fatalf("duplicate not queued idempotently: %v", err)
		}
	}
	all, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(all) != 40 {
		t.Fatalf("queue=%d %v", len(all), err)
	}
	exhausted := errors.New("QUOTA_BLOCKED synthetic authority")
	f.manager.SetAdmissionDispatchCheck(func(context.Context) error { return exhausted })
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); !errors.Is(err, exhausted) {
		t.Fatal(err)
	}
	all, err = f.manager.ListAdmissionQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range all {
		if request.Generation != 0 || request.State != "queued" {
			t.Fatal("lockout changed queue")
		}
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	f.manager.clock = func() time.Time { return now }
	f.manager.SetAdmissionDispatchCheck(func(context.Context) error { return exhausted })
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); !errors.Is(err, exhausted) {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatalf("lockout spawned processes: %d", live)
	}
	if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
		t.Fatal(err)
	}
	// Fake authority reset is explicit; a clock change alone did not open it.
	f.manager.SetAdmissionDispatchCheck(func(context.Context) error { return nil })
	completed := make(map[domain.SessionID]bool)
	projects := make(map[domain.ProjectID]bool)
	for range 40 {
		now = now.Add(5 * time.Second)
		if err := f.manager.DispatchAdmissionQueue(ctx, 64); err != nil {
			t.Fatal(err)
		}
		rows, err := f.store.ListAllSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		active := 0
		for _, row := range rows {
			if row.IsTerminated {
				continue
			}
			active++
			if completed[row.ID] {
				t.Fatal("duplicated task session", row.ID)
			}
			completed[row.ID] = true
			projects[row.ProjectID] = true
			if _, err := f.manager.Kill(ctx, row.ID); err != nil {
				t.Fatal(err)
			}
		}
		if active != 1 {
			t.Fatalf("active=%d, want one bounded worker", active)
		}
	}
	if len(completed) != 40 || len(projects) != 3 {
		t.Fatal(len(completed), len(projects))
	}
	all, err = f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(all) != 40 {
		t.Fatal(len(all), err)
	}
	for _, request := range all {
		if request.State != "completed" || request.Attempts != 0 {
			t.Fatalf("bad final request %+v", request)
		}
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); err != nil {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatal("completed commands replayed", live)
	}
}

func TestAdmissionCrashAfterLaunchBeforeAckDoesNotDuplicate(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := admissionSpawn("blue")
	cfg.ClientRequestID = "stable-task"
	cfg.ClientRequestHash = "stable-hash"
	if _, _, _, err := f.manager.Spawn(ctx, cfg); !errors.Is(err, ErrSessionQueued) {
		t.Fatal(err)
	}
	if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
		t.Fatal(err)
	}
	request, ok, err := f.store.ClaimSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	commandCtx := context.WithValue(ctx, admissionQueueContextKey{}, request.ID)
	started, _, _, err := f.manager.Spawn(commandCtx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Crash here: there is no queue acknowledgement. The native runtime and
	// durable committed session survive.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	if err := f.store.RecoverSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	all, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(all) != 1 || all[0].State != "completed" || all[0].SessionID != started.ID {
		t.Fatalf("queue %+v %v", all, err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatalf("duplicate process after crash: %d", live)
	}
}

func TestAdmissionRestoreContendersAndDirtyPreservation(t *testing.T) {
	f := newAdmissionFixture(t, 40, false)
	ctx := context.Background()
	var ids []domain.SessionID
	for i := range 40 {
		rec, _, _, err := f.manager.Spawn(ctx, admissionSpawn([]domain.ProjectID{"red", "blue", "green"}[i%3]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rec.Metadata.WorkspacePath, "owner-dirty.txt"), []byte("preserved fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.manager.Kill(ctx, rec.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	f.manager.SetWorkerResourceCeiling(8)
	results := make(chan error, 64)
	start := make(chan struct{})
	for i := range 64 {
		go func() { <-start; _, err := f.manager.RestoreWithMode(ctx, ids[i%len(ids)]); results <- err }()
	}
	close(start)
	success := 0
	for range 64 {
		err := <-results
		if err == nil {
			success++
		} else if !errors.Is(err, ErrWorkerCapacity) && !errors.Is(err, ErrOperationInProgress) && !errors.Is(err, ErrNotRestorable) {
			t.Fatal(err)
		}
	}
	if success > 8 || success == 0 {
		t.Fatalf("restore cap success=%d", success)
	}
	snapshot, err := f.manager.AdmissionSnapshot(ctx)
	if err != nil || snapshot.ActiveWorkerSessions > 8 {
		t.Fatal(snapshot, err)
	}
	rows, err := f.store.ListAllSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		mtechAssertDirtyContent(t, filepath.Join(row.Metadata.WorkspacePath, "owner-dirty.txt"), []byte("preserved fixture"))
	}
	if f.ws.forceCalls.Load() != 0 {
		t.Fatal("dirty workspace forced")
	}
}

func TestAdmissionProjectCapResourcePauseAndCoordinatorSingularity(t *testing.T) {
	f := newAdmissionFixture(t, 40, false)
	ctx := context.Background()
	f.manager.admission.limits.ProjectWorkers = map[domain.ProjectID]int{"red": 1}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red")); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal(err)
	}
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue")); err != nil {
		t.Fatal(err)
	}
	f.manager.SetWorkerResourceCeiling(0)
	if _, _, _, err := f.manager.Spawn(ctx, admissionSpawn("green")); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 64)
	releases := make(chan func(), 64)
	for range 64 {
		go func() {
			<-start
			_, _, release, err := f.manager.reserveSessionAdmission(ctx, "green", domain.KindOrchestrator, "")
			if err == nil {
				releases <- release
			}
			results <- err
		}()
	}
	close(start)
	admitted := 0
	for range 64 {
		err := <-results
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrOrchestratorOccupied) {
			t.Fatal(err)
		}
	}
	if admitted != 1 {
		t.Fatalf("coordinator reservations=%d", admitted)
	}
	(<-releases)()
}

func TestAdmissionAmbiguousCrashBlocksWithoutAnotherLaunch(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := admissionSpawn("blue")
	cfg.ClientRequestID, cfg.ClientRequestHash = "incomplete-task", "incomplete-hash"
	if _, _, _, err := f.manager.Spawn(ctx, cfg); !errors.Is(err, ErrSessionQueued) {
		t.Fatal(err)
	}
	if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
		t.Fatal(err)
	}
	request, ok, err := f.store.ClaimSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	project, _, err := f.store.GetProject(ctx, "blue")
	if err != nil {
		t.Fatal(err)
	}
	seed := seedRecord(cfg, project.Config, time.Now().UTC())
	if _, _, err := f.store.CreateClientRequestSession(ctx, seed); err != nil {
		t.Fatal(err)
	}
	f.rebuild() // old controller ended before confirming whether it launched
	if err := f.store.RecoverSessionAdmission(ctx, f.manager.admission.owner, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	all, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil || len(all) != 1 || all[0].ID != request.ID || all[0].State != "blocked" || all[0].FailureCode != "OUTCOME_REQUIRES_RECONCILIATION" {
		t.Fatalf("ambiguous command %+v %v", all, err)
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatalf("ambiguous command launched %d processes", live)
	}
}

func TestAdmissionDependenciesAndCancellation(t *testing.T) {
	f := newAdmissionFixture(t, 1, true)
	ctx := context.Background()
	occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	first := admissionSpawn("blue")
	first.ClientRequestID, first.ClientRequestHash = "dependency", "dependency-hash"
	_, _, _, err = f.manager.Spawn(ctx, first)
	var queued *SessionQueuedError
	if !errors.As(err, &queued) {
		t.Fatal(err)
	}
	dependency := queued.RequestID
	child := admissionSpawn("green")
	child.AdmissionDependencies = []string{dependency}
	child.ClientRequestID, child.ClientRequestHash = "dependent", "dependent-hash"
	_, _, _, err = f.manager.Spawn(ctx, child)
	if !errors.As(err, &queued) {
		t.Fatal(err)
	}
	childID := queued.RequestID
	if err := f.manager.admissionDependenciesReady(ctx, child.AdmissionDependencies); !errors.Is(err, ErrAdmissionDependencyPending) {
		t.Fatal(err)
	}
	if ok, err := f.manager.CancelAdmissionRequest(ctx, dependency); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); err != nil {
		t.Fatal(err)
	}
	all, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		if row.ID == childID && row.State != "blocked" {
			t.Fatalf("child ignored failed dependency: %+v", row)
		}
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatal("cancelled/dependent task launched", live)
	}
	child.AdmissionDependencies = []string{"unknown"}
	if _, _, _, err := f.manager.Spawn(ctx, child); err == nil {
		t.Fatal("unknown dependency accepted")
	}
}

func TestAdmissionWorkspaceOwnerConflictDoesNotRemoveDirtyTree(t *testing.T) {
	f := newAdmissionFixture(t, 2, false)
	ctx := context.Background()
	owner, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(owner.Metadata.WorkspacePath, "owner-dirty.txt")
	if err := os.WriteFile(path, []byte("keep original writer"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, bind, release, err := f.manager.reserveSessionAdmission(ctx, "blue", domain.KindWorker, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	bind("other-session")
	if err := f.manager.reserveWorkspaceOwner(ctx, "other-session", owner.Metadata.WorkspacePath); !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatal(err)
	}
	mtechAssertDirtyContent(t, path, []byte("keep original writer"))
	if f.ws.forceCalls.Load() != 0 {
		t.Fatal("ownership check performed force cleanup")
	}
}

func TestAdmissionResourcePauseBlocksStoppedWorkerRestart(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	row, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Destroy(ctx, ports.RuntimeHandle{ID: row.Metadata.RuntimeHandleID}); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stopped, found, err := f.store.GetSession(ctx, row.ID)
	if err != nil || !found || !stopped.RuntimeReleased() {
		t.Fatal(stopped, found, err)
	}
	f.manager.SetWorkerResourceCeiling(0)
	if _, err := f.manager.ResumeAgentWithMode(ctx, row.ID); !errors.Is(err, ErrWorkerCapacity) {
		t.Fatal("stopped resume bypassed pressure ceiling", err)
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatal("resource pause launched process", live)
	}
	f.manager.SetWorkerResourceCeiling(4)
	if _, err := f.manager.ResumeAgentWithMode(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatal(live)
	}
}

func TestAdmissionRejectsUnknownSessionKind(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	cfg := admissionSpawn("red")
	cfg.Kind = ""
	if _, _, _, err := f.manager.Spawn(context.Background(), cfg); err == nil {
		t.Fatal("unknown kind bypassed worker accounting")
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatal(live)
	}
}

func TestAdmissionQueuedRestoreUsesLifecycleGeneration(t *testing.T) {
	for _, changedEpisode := range []bool{false, true} {
		t.Run(fmt.Sprintf("changedEpisode=%t", changedEpisode), func(t *testing.T) {
			f := newAdmissionFixture(t, 1, true)
			ctx := context.Background()
			archived, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
			if err != nil {
				t.Fatal(err)
			}
			dirty := filepath.Join(archived.Metadata.WorkspacePath, "owner-dirty.txt")
			if err := os.WriteFile(dirty, []byte("queued restore work remains owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.Kill(ctx, archived.ID); err != nil {
				t.Fatal(err)
			}
			occupied, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.RestoreWithMode(ctx, archived.ID); !errors.Is(err, ErrSessionQueued) {
				t.Fatal(err)
			}
			queuedBefore, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(queuedBefore) != 1 {
				t.Fatal(queuedBefore, err)
			}
			renamed, found, err := f.store.GetSession(ctx, archived.ID)
			if err != nil || !found {
				t.Fatal(err)
			}
			renamed.DisplayName = "renamed while capacity is full"
			renamed.UpdatedAt = renamed.UpdatedAt.Add(time.Second)
			if err := f.store.UpdateSession(ctx, renamed); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.RestoreWithMode(ctx, archived.ID); !errors.Is(err, ErrSessionQueued) {
				t.Fatal(err)
			}
			requests, err := f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(requests) != 1 || requests[0].ID != queuedBefore[0].ID {
				t.Fatal("metadata edit changed restore identity", requests, err)
			}
			if _, err := f.manager.Kill(ctx, occupied.ID); err != nil {
				t.Fatal(err)
			}
			if changedEpisode {
				if _, err := f.manager.RestoreWithMode(ctx, archived.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.manager.Kill(ctx, archived.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.manager.DispatchAdmissionQueue(ctx, 1); err != nil {
				t.Fatal(err)
			}
			requests, err = f.manager.ListAdmissionQueue(ctx)
			if err != nil || len(requests) != 1 {
				t.Fatal(requests, err)
			}
			if changedEpisode {
				if requests[0].State != "blocked" {
					t.Fatal("stale episode resurrected", requests)
				}
				if live, _ := f.rt.counts(); live != 0 {
					t.Fatal(live)
				}
			} else {
				if requests[0].State != "completed" || requests[0].SessionID != archived.ID {
					t.Fatal("metadata rename blocked valid restore", requests)
				}
				if live, _ := f.rt.counts(); live != 1 {
					t.Fatal(live)
				}
			}
			mtechAssertDirtyContent(t, dirty, []byte("queued restore work remains owned"))
		})
	}
}
