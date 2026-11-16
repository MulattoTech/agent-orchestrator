//go:build windows

package sessionmanager

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/google/uuid"
)

type admissionNativeProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	done       chan struct{}
	once       sync.Once
	session    domain.SessionID
	generation string
}
type admissionNativeRuntime struct {
	mu         sync.Mutex
	root       string
	executable string
	processes  map[string]*admissionNativeProcess
}

func (r *admissionNativeRuntime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	token := uuid.NewString()
	ready := filepath.Join(r.root, token+".ready")
	// cfg.Command is deliberately ignored: even a test regression cannot
	// accidentally invoke the selected production harness.
	cmd := exec.Command(r.executable)
	cmd.Dir = r.root
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"), "GOMAXPROCS=1",
		"AO_ADMISSION_NATIVE_HELPER=1", "AO_ADMISSION_HELPER_READY=" + ready,
		"AO_ADMISSION_HELPER_TOKEN=" + token,
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return ports.RuntimeHandle{}, err
	}
	p := &admissionNativeProcess{cmd: cmd, stdin: stdin, done: make(chan struct{}), session: cfg.SessionID, generation: cfg.Env[EnvRuntimeLaunchID]}
	go func() { _ = cmd.Wait(); close(p.done) }()
	id := fmt.Sprintf("native-fixture-%d-%s", cmd.Process.Pid, token)
	r.mu.Lock()
	r.processes[id] = p
	r.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		bytes, err := os.ReadFile(ready)
		if err == nil && string(bytes) == token {
			return ports.RuntimeHandle{ID: id}, nil
		}
		select {
		case <-ctx.Done():
			_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: id})
			return ports.RuntimeHandle{}, ctx.Err()
		case <-p.done:
			return ports.RuntimeHandle{}, errors.New("native fixture exited before ready")
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-p.done
			return ports.RuntimeHandle{}, errors.New("native fixture readiness timeout")
		case <-tick.C:
		}
	}
}

func (r *admissionNativeRuntime) lookup(id string) *admissionNativeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.processes[id]
}
func (r *admissionNativeRuntime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	p := r.lookup(handle.ID)
	if p == nil {
		return errors.New("unowned native fixture handle refused")
	}
	p.once.Do(func() { _ = p.stdin.Close() })
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *admissionNativeRuntime) IsAlive(_ context.Context, handle ports.RuntimeHandle) (bool, error) {
	p := r.lookup(handle.ID)
	if p == nil {
		return false, nil
	}
	select {
	case <-p.done:
		return false, nil
	default:
		return true, nil
	}
}
func (*admissionNativeRuntime) GetOutput(context.Context, ports.RuntimeHandle, int) (string, error) {
	return "", nil
}
func (r *admissionNativeRuntime) ProbeFencedRuntime(ctx context.Context, ref ports.FencedRuntimeRef) ports.FencedProbeResult {
	p := r.lookup(ref.Handle.ID)
	if p == nil {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonGenerationMismatch}
	}
	if p.session != ref.SessionID || p.generation != ref.Generation {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonGenerationMismatch}
	}
	live, _ := r.IsAlive(ctx, ref.Handle)
	if live {
		return ports.FencedProbeResult{Liveness: ports.FencedAlive, Reason: ports.FencedReasonExactMatch}
	}
	return ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}
}
func (r *admissionNativeRuntime) closeAll() {
	r.mu.Lock()
	all := make([]*admissionNativeProcess, 0, len(r.processes))
	for _, p := range r.processes {
		all = append(all, p)
	}
	r.mu.Unlock()
	for _, p := range all {
		p.once.Do(func() { _ = p.stdin.Close() })
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
}
func (r *admissionNativeRuntime) livePIDs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pids []int
	for _, p := range r.processes {
		select {
		case <-p.done:
		default:
			pids = append(pids, p.cmd.Process.Pid)
		}
	}
	return pids
}

// Linked only into the packaged offline companion. Its fixed sibling helper
// must match this exact build-time digest; arbitrary executable paths are not
// accepted. Ordinary source tests compile the standard-library fixture locally.
var admissionPackagedHelperSHA256 string

func TestAdmissionNativeWindowsFortyProcessesRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	f := newAdmissionFixture(t, 40, false)
	native := &admissionNativeRuntime{root: t.TempDir(), processes: make(map[string]*admissionNativeProcess)}
	if admissionPackagedHelperSHA256 != "" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		native.executable = filepath.Join(filepath.Dir(self), "ao-offline-worker.exe")
		bytes, err := os.ReadFile(native.executable)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(bytes)
		if fmt.Sprintf("%x", digest) != admissionPackagedHelperSHA256 {
			t.Fatal("packaged offline helper digest mismatch")
		}
		t.Logf("NATIVE_FAKE_HELPER packaged_sha256=%s", admissionPackagedHelperSHA256)
	} else {
		native.executable = filepath.Join(native.root, "admission-worker.exe")
		build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-trimpath", "-o", native.executable, "./testdata/admission-worker/main.go")
		build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOENV=off", "GOFLAGS=", "CGO_ENABLED=0", "GOMAXPROCS=2")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build stdlib-only helper: %v %s", err, output)
		}
	}
	t.Cleanup(native.closeAll)
	f.manager.runtime = native
	f.manager.backgroundContext = ctx
	var sessions []domain.SessionRecord
	for _, stage := range []int{2, 4, 8, 16, 20, 30, 40} {
		for len(sessions) < stage {
			if sys, err := procmem.ReadSystem(); err == nil && sys.AvailableBytes < 1<<30 {
				t.Fatal("native fixture stopped below 1 GiB available RAM")
			}
			rec, _, _, err := f.manager.Spawn(ctx, admissionSpawn([]domain.ProjectID{"red", "blue", "green"}[len(sessions)%3]))
			if err != nil {
				t.Fatal(err)
			}
			sessions = append(sessions, rec)
		}
		pids := native.livePIDs()
		if len(pids) != stage {
			t.Fatalf("native process count=%d stage=%d", len(pids), stage)
		}
		table, err := procmem.Snapshot(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		tree := table.Tree(pids...)
		if len(tree.Processes) != stage {
			t.Fatalf("Windows native process snapshot=%d want=%d", len(tree.Processes), stage)
		}
		if tree.RSSBytes > 768<<20 {
			t.Fatalf("native fake-process memory budget exceeded: %d", tree.RSSBytes)
		}
		sys, err := procmem.ReadSystem()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("NATIVE_FAKE_FLEET slots=%d processes=%d projects=3 rss_bytes=%d cpu_seconds=%.4f available_bytes=%d pids=%v provider_calls=0", stage, len(tree.Processes), tree.RSSBytes, tree.CPUSeconds, sys.AvailableBytes, pids)
	}
	victim := sessions[0]
	dirtyPath := filepath.Join(victim.Metadata.WorkspacePath, "owner-dirty.txt")
	before := []byte("Native Windows fake worker dirty-content preservation\n")
	if err := os.WriteFile(dirtyPath, before, 0600); err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)
	old := native.lookup(victim.Metadata.RuntimeHandleID)
	oldPID := old.cmd.Process.Pid
	// An invented/stale handle cannot terminate one of the registered children.
	if err := native.Destroy(ctx, ports.RuntimeHandle{ID: fmt.Sprintf("native-fixture-%d-stale", oldPID)}); err == nil {
		t.Fatal("stale ownership accepted")
	}
	if !func() bool {
		alive, _ := native.IsAlive(ctx, ports.RuntimeHandle{ID: victim.Metadata.RuntimeHandleID})
		return alive
	}() {
		t.Fatal("stale handle terminated child")
	}
	if err := old.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-old.done
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	f.manager.runtime = native
	f.manager.backgroundContext = ctx
	if err := f.manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stopped, found, err := f.store.GetSession(ctx, victim.ID)
	if err != nil || !found || stopped.Activity.State != domain.ActivityExited || stopped.IsTerminated || !stopped.RuntimeReleased() {
		t.Fatalf("stopped state %+v found=%t err=%v", stopped, found, err)
	}
	if got := len(native.livePIDs()); got != 39 {
		t.Fatalf("restart launched an unrequested process: %d", got)
	}
	result, err := f.manager.ResumeAgentWithMode(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := native.lookup(result.Session.Metadata.RuntimeHandleID)
	if replacement == nil || replacement.cmd.Process.Pid == oldPID || result.Session.Metadata.RuntimeLaunchID == victim.Metadata.RuntimeLaunchID {
		t.Fatal("resume reused retired process identity")
	}
	after, err := os.ReadFile(dirtyPath)
	if err != nil || sha256.Sum256(after) != beforeHash {
		t.Fatal("dirty content changed", err)
	}
	if len(native.livePIDs()) != 40 {
		t.Fatal("resume did not restore exactly one owned process")
	}
	t.Logf("NATIVE_FAKE_RECOVERY old_pid=%d new_pid=%d dirty_sha256=%x all_other_children_preserved=true provider_calls=0", oldPID, replacement.cmd.Process.Pid, beforeHash)
}
