package sessionmanager

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func waitWorkspaceScriptWaiters(t *testing.T, gate *workspaceScriptGate, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gate.snapshot().WaitingShells == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d waiters: %+v", n, gate.snapshot())
}

func TestWorkspaceScriptAdmissionContentionAndCancellation(t *testing.T) {
	gate := newWorkspaceScriptGate(func(string) string { return "" })
	red, err := gate.acquire(t.Context(), "red")
	if err != nil {
		t.Fatal(err)
	}
	defer red()
	blue, err := gate.acquire(t.Context(), "blue")
	if err != nil {
		t.Fatal(err)
	}
	defer blue()
	blockedCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		release, err := gate.acquire(blockedCtx, "red")
		if release != nil {
			release()
		}
		blocked <- err
	}()
	green := make(chan func(), 1)
	greenErrors := make(chan error, 1)
	go func() {
		release, err := gate.acquire(t.Context(), "green")
		if err != nil {
			greenErrors <- err
			return
		}
		green <- release
	}()
	waitWorkspaceScriptWaiters(t, gate, 2)
	blue()
	var releaseGreen func()
	select {
	case releaseGreen = <-green:
		defer releaseGreen()
	case err := <-greenErrors:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("a project at its cap blocked an eligible project")
	}
	if snapshot := gate.snapshot(); snapshot.ActiveShells != 2 || snapshot.ProjectShells["red"] != 1 || snapshot.ProjectShells["green"] != 1 {
		t.Fatalf("incorrect cap occupancy: %+v", snapshot)
	}
	cancel()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled waiter acquired a shell", err)
	}
	releaseGreen()
	red()
	if snapshot := gate.snapshot(); snapshot.ActiveShells != 0 || snapshot.WaitingShells != 0 || len(snapshot.ProjectShells) != 0 {
		t.Fatalf("release leaked occupancy: %+v", snapshot)
	}
}

func TestWorkspaceScriptAdmissionConcurrentBounds(t *testing.T) {
	gate := newWorkspaceScriptGate(func(string) string { return "" })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 40)
	var counters sync.Mutex
	active, peak := 0, 0
	projects := make(map[domain.ProjectID]int)
	for i := range 40 {
		go func() {
			<-start
			project := []domain.ProjectID{"red", "blue", "green"}[i%3]
			release, err := gate.acquire(ctx, project)
			if err != nil {
				results <- err
				return
			}
			counters.Lock()
			active++
			projects[project]++
			peak = max(peak, active)
			if active > 2 || projects[project] > 1 {
				err = errors.New("workspace shell cap exceeded")
			}
			counters.Unlock()
			time.Sleep(time.Millisecond)
			counters.Lock()
			active--
			projects[project]--
			counters.Unlock()
			release()
			results <- err
		}()
	}
	close(start)
	for range 40 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if peak != 2 || gate.snapshot().ActiveShells != 0 {
		t.Fatalf("contended shell occupancy peak=%d final=%+v", peak, gate.snapshot())
	}
}

func TestWorkspaceScriptAdmissionFailureReleaseAndNoRetry(t *testing.T) {
	gate := newWorkspaceScriptGate(func(string) string { return "1" })
	ctx := context.WithValue(t.Context(), workspaceScriptGateKey{}, gate)
	ctx = withWorkspaceScriptProject(ctx, "red")
	workspace := t.TempDir()
	held, err := gate.acquire(ctx, "red")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	blockedCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if err := runWorkspaceStep(blockedCtx, workspace, "echo should-not-start > forbidden.txt", nil, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked shell did not honor cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "forbidden.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capacity waiter started a shell", err)
	}
	held()
	command := "echo attempted >> attempts.txt; exit 7"
	if runtime.GOOS == "windows" {
		command = "echo attempted >> attempts.txt & exit /b 7"
	}
	err = runPostCreate(ctx, workspace, t.TempDir(), []string{command, "echo later > later.txt"}, nil, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("real shell failure did not propagate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "attempts.txt"))
	if err != nil || strings.Count(string(data), "attempted") != 1 {
		t.Fatalf("side-effecting script was retried: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "later.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("later step ran after failure", err)
	}
	if snapshot := gate.snapshot(); snapshot.ActiveShells != 0 || snapshot.WaitingShells != 0 {
		t.Fatalf("failed command leaked a permit: %+v", snapshot)
	}
	if err := runWorkspaceStep(ctx, workspace, "echo next-command", nil, io.Discard); err != nil {
		t.Fatal("released slot was unusable", err)
	}
}

func TestWorkspaceScriptAdmissionInvalidPolicyFailsClosed(t *testing.T) {
	for _, scenario := range []struct{ global, project string }{{"0", "1"}, {"41", "1"}, {"garbled", "1"}, {"2", "0"}, {"2", "3"}} {
		t.Run(scenario.global+"_"+scenario.project, func(t *testing.T) {
			gate := newWorkspaceScriptGate(func(key string) string {
				if key == "AO_MAX_WORKSPACE_SCRIPTS" {
					return scenario.global
				}
				return scenario.project
			})
			ctx := context.WithValue(t.Context(), workspaceScriptGateKey{}, gate)
			workspace := t.TempDir()
			if err := runWorkspaceStep(ctx, workspace, "echo forbidden > forbidden.txt", nil, io.Discard); !errors.Is(err, ErrWorkspaceScriptPolicy) {
				t.Fatal("invalid policy reached command dispatch", err)
			}
			if _, err := os.Stat(filepath.Join(workspace, "forbidden.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid policy launched a shell", err)
			}
			if snapshot := gate.snapshot(); snapshot.ActiveShells != 0 || snapshot.ConfigurationError == "" || snapshot.DescendantProcesses != nil {
				t.Fatalf("invalid policy snapshot: %+v", snapshot)
			}
		})
	}
	limits, err := workspaceScriptLimitsFromEnv(func(string) string { return "4" })
	if err != nil || limits.global != 4 || limits.project != 4 {
		t.Fatal("valid explicit controls refused", limits, err)
	}
}
