//go:build windows

package conpty

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The responder is deliberately another session. No real process is started
// or stopped: both the PTY and the destructive hooks are in-process fakes.
func TestRecoveredUnqualifiedHostCannotProbeAttachOrDestroy(t *testing.T) {
	isolateRegistry(t)
	foreign := startInProcHost(t, "foreign-session", livePID())
	defer foreign.cleanup(t)
	if err := ptyregistry.Register(context.Background(), ptyregistry.Entry{
		SessionID: "expected-session", PtyHostPID: livePID(),
		PipePath: foreign.addr, LaunchID: "expected-generation",
	}); err != nil {
		t.Fatal(err)
	}
	spawnCalls, killCalls := 0, 0
	rt := New(Options{Spawner: func(context.Context, string, string, []string, map[string]string, HostOptions) (string, int, error) {
		spawnCalls++
		return "", 0, errors.New("fake spawn must not run")
	}})
	rt.killHost = func(string) error { killCalls++; return nil }
	rt.pidIsAlive = func(int) bool { return false }
	handle := ports.RuntimeHandle{ID: "expected-session"}
	probe := rt.ProbeFencedRuntime(context.Background(), ports.FencedRuntimeRef{
		Handle: handle, SessionID: "expected-session", Generation: "expected-generation",
	})
	if probe.Liveness != ports.FencedUnknown {
		t.Errorf("foreign responder probe = %+v; want UNKNOWN", probe)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := rt.Attach(ctx, handle, 24, 80)
	if stream != nil {
		_ = stream.Close()
	}
	if !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Errorf("unqualified attach = %v; want inconclusive", err)
	}
	if err := rt.Destroy(context.Background(), handle); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Errorf("unqualified destroy = %v; want inconclusive", err)
	}
	if killCalls != 0 {
		t.Errorf("sent %d destructive requests to foreign responder", killCalls)
	}
	_, _ = rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "expected-session", WorkspacePath: t.TempDir(), Argv: []string{"offline-helper"},
	})
	if spawnCalls != 0 {
		t.Errorf("started %d replacement hosts while ownership was unknown", spawnCalls)
	}
	entries, _, err := ptyregistry.Scan(context.Background())
	if err != nil || len(entries) != 1 {
		t.Errorf("unknown ownership lost: entries=%v err=%v", entries, err)
	}
}

type fakeOwnedHostProcess struct {
	alive  *atomic.Bool
	kills  *atomic.Int32
	closes *atomic.Int32
}

func (p *fakeOwnedHostProcess) Alive() (bool, error) { return p.alive.Load(), nil }
func (p *fakeOwnedHostProcess) Kill() error          { p.kills.Add(1); p.alive.Store(false); return nil }
func (p *fakeOwnedHostProcess) Close() error         { p.closes.Add(1); return nil }

type countingOwnershipPTY struct {
	*fakePTY
	inputMu sync.Mutex
	input   bytes.Buffer
}

func (p *countingOwnershipPTY) Write(data []byte) (int, error) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	return p.input.Write(data)
}
func (p *countingOwnershipPTY) inputText() string {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	return p.input.String()
}

type ownershipFixture struct {
	host      *host
	pty       *countingOwnershipPTY
	address   string
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func startOwnershipFixture(t *testing.T, address string, identity HostIdentity) *ownershipFixture {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	pty := &countingOwnershipPTY{fakePTY: newFakePTY(77)}
	ring := NewRing()
	ring.Append([]byte("owned replay"))
	host := newHost(ServeConfig{SessionID: identity.SessionID, Identity: identity, Listener: listener, PTY: pty, Ring: ring})
	ctx, cancel := context.WithCancel(context.Background())
	f := &ownershipFixture{host: host, pty: pty, address: listener.Addr().String(), cancel: cancel, done: make(chan struct{})}
	go func() { _ = host.run(ctx); close(f.done) }()
	t.Cleanup(func() { f.stop(t) })
	return f
}
func (f *ownershipFixture) stop(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		f.cancel()
		select {
		case <-f.done:
		case <-time.After(2 * time.Second):
			t.Error("fake ownership host did not stop")
		}
	})
}

func ownershipIdentity() HostIdentity {
	return HostIdentity{SessionID: "owned-session", LaunchID: "owned-host-launch", RuntimeLaunchID: "owned-runtime-generation", PID: livePID(), CreatedAt: "0000000000001234"}
}
func recoveredOwnershipRuntime(t *testing.T, identity HostIdentity, address string) (*Runtime, *atomic.Bool, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	isolateRegistry(t)
	if err := ptyregistry.Register(context.Background(), ptyregistry.Entry{
		SessionID: identity.SessionID, PtyHostPID: identity.PID, PipePath: address,
		LaunchID: identity.RuntimeLaunchID, HostLaunchID: identity.LaunchID, HostCreatedAt: identity.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	alive, kills, closes := &atomic.Bool{}, &atomic.Int32{}, &atomic.Int32{}
	alive.Store(true)
	rt := New(Options{})
	rt.openHostProcess = func(pid int, created string, terminate bool) (ownedHostProcess, error) {
		if pid != identity.PID || created != identity.CreatedAt {
			return nil, errors.New("fake OS creation mismatch")
		}
		return &fakeOwnedHostProcess{alive: alive, kills: kills, closes: closes}, nil
	}
	rt.pidIsAlive = func(int) bool { panic("PID-only liveness must not authorize owned operations") }
	rt.processFinder = func(int) (processKiller, error) { panic("must never re-find a PID for force-stop") }
	rt.destroyWait = 0
	return rt, alive, kills, closes
}

func TestRecoveredHostRejectsForeignSessionGenerationAndCreation(t *testing.T) {
	for _, dimension := range []string{"session", "host-generation", "runtime-generation", "host-pid", "process-creation"} {
		t.Run(dimension, func(t *testing.T) {
			expected := ownershipIdentity()
			actual := expected
			switch dimension {
			case "session":
				actual.SessionID = "foreign-session"
			case "host-generation":
				actual.LaunchID = "foreign-launch"
			case "runtime-generation":
				actual.RuntimeLaunchID = "foreign-generation"
			case "host-pid":
				actual.PID++
			case "process-creation":
				actual.CreatedAt = "0000000000005678"
			}
			foreign := startOwnershipFixture(t, "127.0.0.1:0", actual)
			rt, _, kills, _ := recoveredOwnershipRuntime(t, expected, foreign.address)
			handle := ports.RuntimeHandle{ID: expected.SessionID}
			ref := ports.FencedRuntimeRef{Handle: handle, SessionID: domain.SessionID(expected.SessionID), Generation: expected.RuntimeLaunchID}
			if probe := rt.ProbeFencedRuntime(t.Context(), ref); probe.Liveness != ports.FencedUnknown {
				t.Errorf("foreign probe=%+v", probe)
			}
			if stream, err := rt.Attach(t.Context(), handle, 24, 80); err == nil {
				_ = stream.Close()
				t.Error("foreign attach allowed")
			}
			for name, operation := range map[string]func() error{
				"input":     func() error { return rt.SendInput(t.Context(), handle, "forbidden") },
				"message":   func() error { return rt.SendMessage(t.Context(), handle, "forbidden") },
				"interrupt": func() error { return rt.Interrupt(t.Context(), handle) },
				"kill":      func() error { return rt.Destroy(t.Context(), handle) },
				"title":     func() error { return rt.WatchTerminalTitle(t.Context(), handle, func(string) {}) },
			} {
				if err := operation(); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
					t.Errorf("%s error=%v, want inconclusive", name, err)
				}
			}
			if _, err := rt.GetOutput(t.Context(), handle, 10); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
				t.Errorf("output=%v", err)
			}
			if _, err := rt.GetStyledOutput(t.Context(), handle, 10); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
				t.Errorf("styled output=%v", err)
			}
			if kills.Load() != 0 || foreign.pty.inputText() != "" {
				t.Fatal("foreign host received a side effect")
			}
			foreign.pty.resizeMu.Lock()
			resizes := len(foreign.pty.resizes)
			foreign.pty.resizeMu.Unlock()
			if resizes != 0 {
				t.Fatalf("foreign host resized %d times", resizes)
			}
			select {
			case <-foreign.done:
				t.Fatal("foreign host shut down")
			default:
			}
			entries, _, err := ptyregistry.Scan(t.Context())
			if err != nil || len(entries) != 1 {
				t.Fatalf("fence evidence lost: %+v %v", entries, err)
			}
		})
	}
}

func TestHostCreationMismatchRefusesBeforeTCPContact(t *testing.T) {
	expected := ownershipIdentity()
	foreign := startOwnershipFixture(t, "127.0.0.1:0", expected)
	rt, _, kills, _ := recoveredOwnershipRuntime(t, expected, foreign.address)
	rt.openHostProcess = func(int, string, bool) (ownedHostProcess, error) {
		return nil, errors.New("PID reused: creation FILETIME differs")
	}
	handle := ports.RuntimeHandle{ID: expected.SessionID}
	if err := rt.SendInput(t.Context(), handle, "forbidden"); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatal(err)
	}
	if err := rt.Destroy(t.Context(), handle); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatal(err)
	}
	if kills.Load() != 0 || foreign.pty.inputText() != "" {
		t.Fatal("creation mismatch caused a side effect")
	}
}

func TestRecoveredExactHostCanReconnectAndDestroyWithOwnedHandle(t *testing.T) {
	expected := ownershipIdentity()
	f := startOwnershipFixture(t, "127.0.0.1:0", expected)
	rt, _, kills, closes := recoveredOwnershipRuntime(t, expected, f.address)
	handle := ports.RuntimeHandle{ID: expected.SessionID}
	ref := ports.FencedRuntimeRef{Handle: handle, SessionID: domain.SessionID(expected.SessionID), Generation: expected.RuntimeLaunchID}
	if probe := rt.ProbeFencedRuntime(t.Context(), ref); probe.Liveness != ports.FencedAlive {
		t.Fatalf("exact probe=%+v", probe)
	}
	for i := 0; i < 3; i++ {
		stream, err := rt.Attach(t.Context(), handle, 24, 80)
		if err != nil {
			t.Fatal(err)
		}
		if got := readUntil(t, stream, "owned replay", time.Second); !strings.Contains(got, "owned replay") {
			t.Fatal(got)
		}
		if _, err := stream.Write([]byte("safe-fixture-input")); err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
	}
	if err := rt.Destroy(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	if kills.Load() != 1 {
		t.Fatalf("exact fake-handle stop calls=%d, want1", kills.Load())
	}
	if closes.Load() < 5 {
		t.Fatalf("process handles not released: %d", closes.Load())
	}
	entries, _, err := ptyregistry.Scan(t.Context())
	if err != nil || len(entries) != 0 {
		t.Fatalf("qualified cleanup failed: %+v %v", entries, err)
	}
}

func TestRecoveredHostPortReuseCannotReusePriorSuccessfulProbe(t *testing.T) {
	expected := ownershipIdentity()
	first := startOwnershipFixture(t, "127.0.0.1:0", expected)
	rt, _, kills, _ := recoveredOwnershipRuntime(t, expected, first.address)
	handle := ports.RuntimeHandle{ID: expected.SessionID}
	ref := ports.FencedRuntimeRef{Handle: handle, SessionID: domain.SessionID(expected.SessionID), Generation: expected.RuntimeLaunchID}
	if probe := rt.ProbeFencedRuntime(t.Context(), ref); probe.Liveness != ports.FencedAlive {
		t.Fatal(probe)
	}
	address := first.address
	first.stop(t)
	foreignID := expected
	foreignID.LaunchID = "new-owner-at-reused-port"
	foreign := startOwnershipFixture(t, address, foreignID)
	if stream, err := rt.Attach(t.Context(), handle, 24, 80); err == nil {
		_ = stream.Close()
		t.Fatal("reused port attached")
	}
	if err := rt.Destroy(t.Context(), handle); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatal(err)
	}
	if kills.Load() != 0 || foreign.pty.inputText() != "" {
		t.Fatal("cached probe authorized new owner")
	}
	if probe := rt.ProbeFencedRuntime(t.Context(), ref); probe.Liveness != ports.FencedUnknown {
		t.Fatal(probe)
	}
}

func TestOwnedHostRejectsUnauthenticatedControlAndOversizedHandshake(t *testing.T) {
	expected := ownershipIdentity()
	f := startOwnershipFixture(t, "127.0.0.1:0", expected)
	for _, kind := range []byte{MsgTerminalInput, MsgResize, MsgGetOutputReq, MsgGetStyledOutputReq, MsgKillReq, MsgWatchForegroundReq, MsgWatchTitleReq} {
		conn, err := net.Dial("tcp", f.address)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		frame, _ := EncodeMessage(kind, []byte("forbidden"))
		_, _ = conn.Write(frame)
		var b [1]byte
		if n, err := conn.Read(b[:]); err == nil || n != 0 {
			t.Errorf("unauthenticated 0x%x received replay/response", kind)
		}
		_ = conn.Close()
	}
	conn, err := net.Dial("tcp", f.address)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte{MsgIdentityReq, 0xff, 0xff, 0xff, 0xff})
	var b [1]byte
	if n, err := conn.Read(b[:]); err == nil || n != 0 {
		t.Fatal("oversized identity frame not refused")
	}
	_ = conn.Close()
	status, alive, err := clientStatusContext(t.Context(), f.address)
	if err != nil || !alive || status.HostIdentity != expected {
		t.Fatalf("read-only identity status=%+v alive=%t err=%v", status, alive, err)
	}
	if f.pty.inputText() != "" {
		t.Fatal("unauthenticated input forwarded")
	}
	select {
	case <-f.done:
		t.Fatal("unauthenticated kill accepted")
	default:
	}
}

func TestWindowsHostCreationFenceUsesKernelProcessIdentity(t *testing.T) {
	created, err := readHostCreation(os.Getpid())
	if err != nil || created == "" {
		t.Fatalf("current process identity=%q %v", created, err)
	}
	process, err := openHostProcess(os.Getpid(), created, false)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	alive, err := process.Alive()
	if err != nil || !alive {
		t.Fatalf("exact process alive=%t %v", alive, err)
	}
	if wrong, err := openHostProcess(os.Getpid(), "0000000000000001", false); err == nil {
		_ = wrong.Close()
		t.Fatal("different creation time accepted")
	}
	// Deliberately no termination operation on this real process.
}

func TestWindowsRegistryRetainsLegacyDeadPIDUntilQualifiedCleanup(t *testing.T) {
	isolateRegistry(t)
	if err := ptyregistry.Register(t.Context(), ptyregistry.Entry{SessionID: "legacy-dead", PtyHostPID: deadPID(), PipePath: "127.0.0.1:1", LaunchID: "old"}); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{})
	entries, complete, err := ptyregistry.Scan(t.Context())
	if err != nil || !complete || len(entries) != 1 {
		t.Fatalf("legacy record pruned: %+v %t %v", entries, complete, err)
	}
	ref := ports.FencedRuntimeRef{Handle: ports.RuntimeHandle{ID: "legacy-dead"}, SessionID: "legacy-dead", Generation: "old"}
	if got := rt.ProbeFencedRuntime(t.Context(), ref); got.Liveness != ports.FencedUnknown {
		t.Fatalf("legacy dead PID=%+v", got)
	}
}

func TestQualifiedRefusedPortNeverForceStopsAliveProcess(t *testing.T) {
	expected := ownershipIdentity()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	rt, _, kills, _ := recoveredOwnershipRuntime(t, expected, address)
	handle := ports.RuntimeHandle{ID: expected.SessionID}
	for name, probe := range map[string]func() (bool, error){
		"host":  func() (bool, error) { return rt.IsAlive(t.Context(), handle) },
		"child": func() (bool, error) { return rt.IsChildAlive(t.Context(), handle) },
		"supervisor": func() (bool, error) {
			return rt.IsSupervisedProcessAlive(t.Context(), handle, ports.SupervisedProcessRef{SessionID: domain.SessionID(expected.SessionID), LaunchID: expected.RuntimeLaunchID})
		},
	} {
		if alive, err := probe(); alive || !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
			t.Fatalf("%s refused-port health=%t %v", name, alive, err)
		}
	}
	if err := rt.Destroy(t.Context(), ports.RuntimeHandle{ID: expected.SessionID}); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatalf("qualified refused-port destroy=%v", err)
	}
	if kills.Load() != 0 {
		t.Fatal("a refused port authorized OS termination")
	}
	entries, _, err := ptyregistry.Scan(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatalf("refused-port ownership lost: %+v %v", entries, err)
	}
}

type pausedExitedHost struct {
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *pausedExitedHost) Alive() (bool, error) {
	p.once.Do(func() { close(p.entered); <-p.resume })
	return false, nil
}
func (p *pausedExitedHost) Kill() error  { return errors.New("paused exited host must not be killed") }
func (p *pausedExitedHost) Close() error { return nil }

func TestRecoveredStaleDestroyCannotUnregisterSuccessorLaunch(t *testing.T) {
	expected := ownershipIdentity()
	old := startOwnershipFixture(t, "127.0.0.1:0", expected)
	first, alive, _, _ := recoveredOwnershipRuntime(t, expected, old.address)
	alive.Store(false)
	second := New(Options{})
	paused := &pausedExitedHost{entered: make(chan struct{}), resume: make(chan struct{})}
	second.openHostProcess = func(int, string, bool) (ownedHostProcess, error) { return paused, nil }
	staleDone := make(chan error, 1)
	go func() { staleDone <- second.Destroy(context.Background(), ports.RuntimeHandle{ID: expected.SessionID}) }()
	select {
	case <-paused.entered:
	case <-time.After(time.Second):
		t.Fatal("stale destroy did not hold old identity")
	}
	if err := first.Destroy(t.Context(), ports.RuntimeHandle{ID: expected.SessionID}); err != nil {
		t.Fatal(err)
	}
	old.stop(t)

	hosts := map[string]*inProcHost{}
	successor := newFakeRuntime(t, hosts, livePID())
	handle, err := successor.Create(t.Context(), ports.RuntimeConfig{SessionID: domain.SessionID(expected.SessionID), WorkspacePath: t.TempDir(), Argv: []string{"offline-fake"}, Env: map[string]string{runtimeLaunchIDEnv: "successor-generation"}})
	if err != nil {
		t.Fatal(err)
	}
	defer hosts[handle.ID].cleanup(t)
	before, _, err := ptyregistry.Scan(t.Context())
	if err != nil || len(before) != 1 || before[0].HostLaunchID == expected.LaunchID {
		t.Fatalf("successor registry=%+v %v", before, err)
	}
	close(paused.resume)
	select {
	case err := <-staleDone:
		if !errors.Is(err, ports.ErrRuntimeProbeInconclusive) || !errors.Is(err, ptyregistry.ErrOwnershipChanged) {
			t.Fatalf("stale removal=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale destroy did not finish")
	}
	after, _, err := ptyregistry.Scan(t.Context())
	if err != nil || len(after) != 1 || after[0] != before[0] {
		t.Fatalf("successor was removed: before=%+v after=%+v err=%v", before, after, err)
	}
	if alive, err := successor.IsAlive(t.Context(), handle); err != nil || !alive {
		t.Fatalf("successor alive=%t %v", alive, err)
	}
}

func TestQualifiedPartialReadyFailureCleansOnlyItsRecordedHost(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	spawn := fakeSpawnerFor(t, hosts, livePID())
	spawnErr := errors.New("synthetic failure after READY")
	rt := New(Options{Spawner: func(ctx context.Context, id, cwd string, argv []string, env map[string]string, opts HostOptions) (string, int, error) {
		address, pid, err := spawn(ctx, id, cwd, argv, env, opts)
		if err != nil {
			return address, pid, err
		}
		return address, pid, spawnErr
	}})
	fenceInProcRuntime(t, rt, hosts)
	_, err := rt.Create(t.Context(), ports.RuntimeConfig{SessionID: "partial-owned", WorkspacePath: t.TempDir(), Argv: []string{"offline-fake"}})
	defer hosts["partial-owned"].cleanup(t)
	var effect ports.RuntimeEffectError
	if !errors.As(err, &effect) || !errors.Is(err, spawnErr) || effect.CleanupOutcome() != ports.RuntimeCleanupSucceeded {
		t.Fatalf("qualified partial cleanup=%v", err)
	}
	entries, _, err := ptyregistry.Scan(t.Context())
	if err != nil || len(entries) != 0 {
		t.Fatalf("qualified cleanup left reservation: %+v %v", entries, err)
	}
}
