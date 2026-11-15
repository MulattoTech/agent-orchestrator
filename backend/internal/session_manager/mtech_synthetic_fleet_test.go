package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// Owner-local qualification only: the actual Manager, lifecycle reducer and
// temporary SQLite store manage fake TUI runtimes in plain temporary folders.
// This never launches an agent executable or validates ConPTY/Git containment.
// Counts below are managed slots, not concurrent model turns or an AO limit.
func TestMTechSyntheticFleetLifecycle(t *testing.T) {
	for _, slots := range []int{1, 4, 8, 16, 32, 40, 64} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			t.Cleanup(cancel)
			started := time.Now()
			var initial runtime.MemStats
			runtime.ReadMemStats(&initial)
			monitorStop := make(chan struct{})
			memoryResult := make(chan uint64, 1)
			go func() {
				peak := initial.HeapAlloc
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var mem runtime.MemStats
					runtime.ReadMemStats(&mem)
					peak = max(peak, mem.HeapAlloc)
					if peak > 512<<20 {
						cancel()
					}
					select {
					case <-monitorStop:
						memoryResult <- peak
						return
					case <-ticker.C:
					}
				}
			}()
			var stopOnce sync.Once
			stopMonitor := func() { stopOnce.Do(func() { close(monitorStop) }) }
			t.Cleanup(stopMonitor)

			store, err := sqlite.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			project := domain.ProjectRecord{ID: "synthetic", Kind: domain.ProjectKindScratch,
				Path: filepath.Join(root, "project"), RegisteredAt: time.Now().UTC(),
				Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessAider}}}
			if err := os.MkdirAll(project.Path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			rt := &mtechFleetRuntime{live: map[string]domain.SessionID{"unowned-sentinel": "external"}}
			ws := &mtechFleetWorkspace{root: filepath.Join(root, "workspaces"), owned: map[string]ports.WorkspaceInfo{}}
			newManager := func() *Manager {
				return New(Deps{Runtime: rt, Agents: fakeAgents{}, Workspace: ws,
					Store: store, Lifecycle: lifecycle.New(store, nil), DataDir: dataDir,
					LookPath: func(string) (string, error) { return os.Executable() },
					Logger:   slog.New(slog.NewTextHandler(io.Discard, nil))})
			}
			manager := newManager()
			spawn := func() (domain.SessionRecord, error) {
				rec, _, _, err := manager.Spawn(ctx, ports.SpawnConfig{ProjectID: "synthetic",
					Kind: domain.KindWorker, Harness: domain.HarnessAider, RequestedMode: domain.SessionModeTUI,
					Prompt: "SYNTHETIC FIXTURE: no executable is run", AgentConfigResolved: true})
				return rec, err
			}
			type outcome struct {
				rec domain.SessionRecord
				err error
			}
			results := make(chan outcome, slots)
			start := make(chan struct{})
			var inFlight, peakInFlight atomic.Int32
			for range slots {
				go func() {
					<-start
					n := inFlight.Add(1)
					for old := peakInFlight.Load(); n > old && !peakInFlight.CompareAndSwap(old, n); old = peakInFlight.Load() {
					}
					rec, err := spawn()
					inFlight.Add(-1)
					results <- outcome{rec, err}
				}()
			}
			close(start)
			sessions := make([]domain.SessionRecord, 0, slots)
			handles := make(map[string]bool, slots)
			var spawnErrors []error
			for range slots {
				result := <-results
				if result.err != nil {
					spawnErrors = append(spawnErrors, result.err)
					continue
				}
				handle := result.rec.Metadata.RuntimeHandleID
				if handle == "" || handles[handle] {
					spawnErrors = append(spawnErrors, fmt.Errorf("missing/duplicate runtime handle %q", handle))
				}
				handles[handle] = true
				sessions = append(sessions, result.rec)
			}
			if len(spawnErrors) > 0 {
				t.Fatalf("spawn: %v", errors.Join(spawnErrors...))
			}
			mtechAssertFleetRows(t, ctx, store, slots, slots)
			if live, external := rt.counts(); live != slots || !external {
				t.Fatalf("live owned=%d external=%t, want %d and true", live, external, slots)
			}

			// An additional failed launch must roll back its seed/workspace and
			// leave all already-running synthetic sessions intact.
			rt.failNext.Store(true)
			if _, err := spawn(); !errors.Is(err, ErrRuntimeCreate) {
				t.Fatalf("injected launch failure: %v", err)
			}
			mtechAssertFleetRows(t, ctx, store, slots, slots)
			if live, _ := rt.counts(); live != slots {
				t.Fatalf("failed launch disturbed live sessions: %d", live)
			}

			// Only the fake adapter's dirty refusal is under test. The sentinel
			// is real local content; Git dirty detection itself is not simulated.
			dirty := sessions[0]
			dirtyPath := filepath.Join(dirty.Metadata.WorkspacePath, "owner-dirty.txt")
			dirtyContent := []byte("preserve this synthetic owner change\n")
			if err := os.WriteFile(dirtyPath, dirtyContent, 0o600); err != nil {
				t.Fatal(err)
			}
			killed := make(chan error, slots)
			for _, rec := range sessions {
				go func() {
					_, err := manager.Kill(ctx, rec.ID)
					killed <- err
				}()
			}
			var killErrors []error
			for range slots {
				if err := <-killed; err != nil {
					killErrors = append(killErrors, err)
				}
			}
			if len(killErrors) > 0 {
				t.Fatalf("kill: %v", errors.Join(killErrors...))
			}
			mtechAssertFleetRows(t, ctx, store, slots, 0)
			mtechAssertDirtyContent(t, dirtyPath, dirtyContent)
			if live, external := rt.counts(); live != 0 || !external {
				t.Fatalf("after kill owned=%d external=%t", live, external)
			}
			if len(ws.owned) != 1 || ws.forceCalls.Load() != 0 {
				t.Fatalf("preserved workspaces=%d force calls=%d", len(ws.owned), ws.forceCalls.Load())
			}

			// Reopen the isolated DB and recreate Manager/reducer to verify this
			// explicit restore uses durable facts after a clean restart.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			manager = newManager()
			restored, err := manager.RestoreWithMode(ctx, dirty.ID)
			if err != nil {
				t.Fatalf("restore after DB reopen: %v", err)
			}
			if restored.Session.ID != dirty.ID || handles[restored.Session.Metadata.RuntimeHandleID] {
				t.Fatal("restore changed session identity or reused a retired runtime handle")
			}
			mtechAssertFleetRows(t, ctx, store, slots, 1)
			mtechAssertDirtyContent(t, dirtyPath, dirtyContent)
			// Remove only this test-created marker so the final teardown may
			// reclaim its own folder; this never touches a user worktree.
			if err := os.Remove(dirtyPath); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Kill(ctx, dirty.ID); err != nil {
				t.Fatal(err)
			}
			mtechAssertFleetRows(t, ctx, store, slots, 0)
			if live, external := rt.counts(); live != 0 || !external || len(ws.owned) != 0 || ws.forceCalls.Load() != 0 {
				t.Fatalf("final ownership: live=%d external=%t workspaces=%d force=%d", live, external, len(ws.owned), ws.forceCalls.Load())
			}
			var diskBytes int64
			if err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() {
					info, err := entry.Info()
					if err != nil {
						return err
					}
					diskBytes += info.Size()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if diskBytes > 128<<20 {
				t.Fatalf("synthetic disk threshold exceeded: %d", diskBytes)
			}
			stopMonitor()
			peakHeap := <-memoryResult
			if peakHeap > 512<<20 || ctx.Err() != nil {
				t.Fatalf("resource/time bound exceeded: heap=%d context=%v", peakHeap, ctx.Err())
			}
			var final runtime.MemStats
			runtime.ReadMemStats(&final)
			report, _ := json.Marshal(map[string]any{
				"classification": "SYNTHETIC_REPRODUCED", "os": runtime.GOOS, "arch": runtime.GOARCH,
				"submitted": slots, "accepted": slots, "injected_failed_launches": 1,
				"peak_inflight_spawn_calls": peakInFlight.Load(), "peak_live_fake_runtimes": slots,
				"final_session_rows": slots, "final_live_fake_runtimes": 0, "unowned_fake_runtime_preserved": true,
				"reopened_db_restores": 1, "dirty_content_preserved": true, "force_destroy_calls": ws.forceCalls.Load(),
				"elapsed_ms": time.Since(started).Milliseconds(), "sampled_peak_go_heap_bytes": peakHeap,
				"go_allocated_bytes": final.TotalAlloc - initial.TotalAlloc, "retained_temp_disk_bytes": diskBytes,
				"model_turns": 0, "real_agent_processes": 0, "os_rss_cpu_handles": "NOT_MEASURED",
			})
			t.Logf("SYNTHETIC_FLEET %s", report)
		})
	}
}

type mtechFleetRuntime struct {
	mu         sync.Mutex
	sequence   int
	live       map[string]domain.SessionID
	generation map[string]string
	failNext   atomic.Bool
}

func (r *mtechFleetRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	if r.failNext.Swap(false) {
		return ports.RuntimeHandle{}, errors.New("synthetic runtime launch failure")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, owner := range r.live {
		if owner == cfg.SessionID {
			return ports.RuntimeHandle{}, errors.New("duplicate live runtime for session")
		}
	}
	r.sequence++
	id := fmt.Sprintf("synthetic-runtime-%d", r.sequence)
	r.live[id] = cfg.SessionID
	if r.generation == nil {
		r.generation = make(map[string]string)
	}
	r.generation[id] = cfg.Env[EnvRuntimeLaunchID]
	return ports.RuntimeHandle{ID: id}, nil
}

func (r *mtechFleetRuntime) Destroy(_ context.Context, handle ports.RuntimeHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if handle.ID == "unowned-sentinel" {
		return errors.New("attempt to terminate unowned synthetic runtime")
	}
	delete(r.live, handle.ID)
	delete(r.generation, handle.ID)
	return nil
}

func (r *mtechFleetRuntime) IsAlive(_ context.Context, handle ports.RuntimeHandle) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, alive := r.live[handle.ID]
	return alive, nil
}

func (*mtechFleetRuntime) GetOutput(context.Context, ports.RuntimeHandle, int) (string, error) {
	return "", nil
}

func (r *mtechFleetRuntime) ProbeFencedRuntime(_ context.Context, ref ports.FencedRuntimeRef) ports.FencedProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, found := r.live[ref.Handle.ID]
	if !found {
		return ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}
	}
	if owner != ref.SessionID || r.generation[ref.Handle.ID] != ref.Generation {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonGenerationMismatch}
	}
	return ports.FencedProbeResult{Liveness: ports.FencedAlive, Reason: ports.FencedReasonExactMatch}
}

func (r *mtechFleetRuntime) counts() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, external := r.live["unowned-sentinel"]
	owned := len(r.live)
	if external {
		owned--
	}
	return owned, external
}

type mtechFleetWorkspace struct {
	mu         sync.Mutex
	root       string
	owned      map[string]ports.WorkspaceInfo
	forceCalls atomic.Int32
}

func (w *mtechFleetWorkspace) Create(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := filepath.Join(w.root, string(cfg.SessionID))
	if err := os.MkdirAll(path, 0o700); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	info := ports.WorkspaceInfo{Path: path, Branch: cfg.Branch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}
	w.owned[path] = info
	return info, nil
}

func (w *mtechFleetWorkspace) Destroy(_ context.Context, info ports.WorkspaceInfo) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	owner, ok := w.owned[info.Path]
	if !ok {
		return nil
	}
	if owner.SessionID != info.SessionID {
		return errors.New("wrong synthetic workspace owner")
	}
	if _, err := os.Stat(filepath.Join(info.Path, "owner-dirty.txt")); err == nil {
		return ports.ErrWorkspaceDirty
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(info.Path); err != nil {
		return err
	}
	delete(w.owned, info.Path)
	return nil
}

func (w *mtechFleetWorkspace) Restore(_ context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.owned[cfg.Path]
	if !ok || info.SessionID != cfg.SessionID {
		return ports.WorkspaceInfo{}, errors.New("missing or differently owned synthetic workspace")
	}
	return info, nil
}

func (w *mtechFleetWorkspace) ForceDestroy(context.Context, ports.WorkspaceInfo) error {
	w.forceCalls.Add(1)
	return errors.New("force removal is forbidden by this fixture")
}

func (*mtechFleetWorkspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	return "", errors.New("Git preservation is not implemented by this fixture")
}

func (*mtechFleetWorkspace) ApplyPreserved(context.Context, ports.WorkspaceInfo, string) error {
	return errors.New("Git preservation is not implemented by this fixture")
}

func (*mtechFleetWorkspace) AddExclude(context.Context, ports.WorkspaceInfo, ...string) error {
	return nil
}

func mtechAssertFleetRows(t *testing.T, ctx context.Context, store *sqlite.Store, total, active int) {
	t.Helper()
	rows, err := store.ListSessions(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, rec := range rows {
		if !rec.IsTerminated {
			live++
		}
	}
	if len(rows) != total || live != active {
		t.Fatalf("session rows=%d live=%d; want %d and %d", len(rows), live, total, active)
	}
}

func mtechAssertDirtyContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("synthetic dirty content changed: %q, %v", got, err)
	}
}
