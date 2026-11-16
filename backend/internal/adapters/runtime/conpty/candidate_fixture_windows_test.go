//go:build windows

package conpty

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

// OfflineCandidateConPTYFixture is link-set only for the separately compiled
// fixture. Its default invocation cannot run unrelated package tests. This is
// test code: no fake provider or alternate production dispatch path is added.
var OfflineCandidateConPTYFixture string

func TestMain(m *testing.M) {
	if OfflineCandidateConPTYFixture == "1" {
		pattern := "^TestActualCandidateConPTYMechanics$"
		if os.Getenv("AO_CONPTY_FIXTURE_OWNER") == "1" {
			pattern = "^TestCandidateConPTYOwnerProcess$"
		}
		os.Args = []string{os.Args[0], "-test.run=" + pattern, "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=90s"}
	}
	os.Exit(m.Run())
}

type candidateFixtureConfig struct {
	Executable       string
	Helper           string
	ExecutableSHA256 string
	HelperSHA256     string
	SourceCommit     string
}

type candidateFixtureIdentity struct {
	PID      int    `json:"pid"`
	Creation uint64 `json:"creationTicks"`
}

type candidateFixtureHandoff struct {
	Session    string                   `json:"session"`
	Generation string                   `json:"generation"`
	Nonce      string                   `json:"nonce"`
	Host       candidateFixtureIdentity `json:"host"`
	Helper     candidateFixtureIdentity `json:"helper"`
	Address    string                   `json:"address"`
}

type candidateOwnedProcess struct {
	handle   windows.Handle
	identity candidateFixtureIdentity
}

func candidateHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func candidateConfig(t *testing.T) candidateFixtureConfig {
	t.Helper()
	c := candidateFixtureConfig{Executable: os.Getenv("AO_CONPTY_CANDIDATE_EXE"), Helper: os.Getenv("AO_CONPTY_HELPER_EXE"),
		ExecutableSHA256: os.Getenv("AO_CONPTY_CANDIDATE_SHA256"), HelperSHA256: os.Getenv("AO_CONPTY_HELPER_SHA256"), SourceCommit: os.Getenv("AO_CONPTY_CANDIDATE_SOURCE")}
	if c == (candidateFixtureConfig{}) && OfflineCandidateConPTYFixture != "1" {
		t.Skip("explicit reviewed candidate/helper paths and exact hashes required")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.SourceCommit) {
		t.Fatal("exact reviewed candidate source commit required")
	}
	for path, digest := range map[string]string{c.Executable: c.ExecutableSHA256, c.Helper: c.HelperSHA256} {
		if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			t.Fatal("absolute local executable and exact SHA256 required")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.EqualFold(filepath.Clean(path), resolved) {
			t.Fatalf("candidate reparse/alias path refused: %v", err)
		}
		actual, err := candidateHashFile(path)
		if err != nil || actual != digest {
			t.Fatalf("candidate executable hash mismatch: %s %v", path, err)
		}
	}
	if filepath.Base(c.Executable) != "AO.exe" || filepath.Base(c.Helper) != "ao-offline-worker.exe" || !strings.EqualFold(filepath.Dir(c.Executable), filepath.Dir(c.Helper)) {
		t.Fatal("fixed candidate/helper filenames in one bundle required")
	}
	bundle := filepath.Dir(c.Executable)
	if !strings.HasPrefix(strings.ToLower(bundle), `c:\aodev\releases\ao-unofficial-`) {
		t.Fatal("candidate must be in the isolated release root")
	}
	var manifest struct {
		SourceCommit string `json:"sourceCommit"`
		StrictPolicy bool   `json:"strictPolicy"`
		Files        map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatal("candidate manifest unavailable")
	}
	manifestHash, err := candidateHashFile(manifestPath)
	checksum, readErr := os.ReadFile(filepath.Join(bundle, "manifest.sha256"))
	if err != nil || readErr != nil || strings.TrimSpace(string(checksum)) != manifestHash || !manifest.StrictPolicy || manifest.SourceCommit != c.SourceCommit || manifest.Files["AO.exe"].SHA256 != c.ExecutableSHA256 || manifest.Files["ao-offline-worker.exe"].SHA256 != c.HelperSHA256 {
		t.Fatal("reviewed candidate does not match its strict build manifest")
	}
	return c
}

func candidateProcess(t *testing.T, identity candidateFixtureIdentity, expectedImage string) *candidateOwnedProcess {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(identity.PID))
	if err != nil {
		t.Fatalf("open exact candidate process: %v", err)
	}
	fail := func(format string, args ...any) { _ = windows.CloseHandle(h); t.Fatalf(format, args...) }
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		fail("read owned executable identity: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buf[:n])), filepath.Clean(expectedImage)) {
		fail("owned executable identity mismatch")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		fail("read owned process generation: %v", err)
	}
	ticks := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if identity.Creation != 0 && identity.Creation != ticks {
		fail("candidate PID was reused")
	}
	return &candidateOwnedProcess{handle: h, identity: candidateFixtureIdentity{identity.PID, ticks}}
}

func (p *candidateOwnedProcess) alive() (bool, error) {
	state, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	if state == uint32(windows.WAIT_TIMEOUT) {
		return true, nil
	}
	if state == windows.WAIT_OBJECT_0 {
		return false, nil
	}
	return false, fmt.Errorf("unexpected owned process wait state %d", state)
}
func (p *candidateOwnedProcess) stop() error {
	alive, err := p.alive()
	if err != nil || !alive {
		return err
	}
	if err := windows.TerminateProcess(p.handle, 77); err != nil {
		return err
	}
	state, err := windows.WaitForSingleObject(p.handle, 3000)
	if err != nil {
		return err
	}
	if state != windows.WAIT_OBJECT_0 {
		return errors.New("owned process did not exit within three seconds")
	}
	return nil
}
func (p *candidateOwnedProcess) close() { _ = windows.CloseHandle(p.handle) }

func candidateEnvironment(c candidateFixtureConfig, root, nonce string, owner bool) []string {
	env := []string{
		"SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("SystemRoot"),
		"PATH=" + filepath.Join(os.Getenv("SystemRoot"), "System32"), "TEMP=" + root, "TMP=" + root, "GOMAXPROCS=2",
		"AO_DATA_DIR=" + root, "AO_RUN_FILE=" + filepath.Join(root, "running.json"),
		"AO_TELEMETRY_EVENTS=off", "AO_TELEMETRY_METRICS=off", "AO_TELEMETRY_REMOTE=off",
		"AO_ADMISSION_NATIVE_HELPER=1", "AO_ADMISSION_HELPER_READY=" + filepath.Join(root, "worker.ready"), "AO_ADMISSION_HELPER_TOKEN=" + nonce,
		"AO_CONPTY_CANDIDATE_EXE=" + c.Executable, "AO_CONPTY_CANDIDATE_SHA256=" + c.ExecutableSHA256, "AO_CONPTY_CANDIDATE_SOURCE=" + c.SourceCommit,
		"AO_CONPTY_HELPER_EXE=" + c.Helper, "AO_CONPTY_HELPER_SHA256=" + c.HelperSHA256,
		"AO_CONPTY_FIXTURE_ROOT=" + root, "AO_CONPTY_FIXTURE_NONCE=" + nonce,
	}
	if owner {
		env = append(env, "AO_CONPTY_FIXTURE_OWNER=1")
	}
	return env
}

func candidateWait(t *testing.T, duration time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func candidateCreate(t *testing.T, c candidateFixtureConfig, root, nonce string) (*Runtime, ports.RuntimeHandle, candidateFixtureHandoff, *candidateOwnedProcess, *candidateOwnedProcess) {
	t.Helper()
	id := "offline-" + nonce
	generation := "generation-" + nonce
	var hostCommand *exec.Cmd
	var address string
	var helperPID int
	stderr := &boundedBuffer{max: 16 << 10}
	rt := New(Options{RunFilePath: filepath.Join(root, "running.json"), Spawner: func(ctx context.Context, session, cwd string, argv []string, env map[string]string, opts HostOptions) (string, int, error) {
		if session != id || cwd != root || len(argv) != 1 || argv[0] != c.Helper || opts.StartOnAttach {
			return "", 0, errors.New("fixture refused non-allowlisted launch")
		}
		hostCommand = exec.Command(c.Executable, "pty-host", id, root, c.Helper)
		hostCommand.Dir = root
		hostCommand.Env = candidateEnvironment(c, root, nonce, false)
		// Only the two runtime-owned identity values enter the otherwise
		// fixed offline environment. No provider/account configuration is copied.
		hostCommand.Env = append(hostCommand.Env,
			hostLaunchIDEnv+"="+env[hostLaunchIDEnv],
			runtimeLaunchIDEnv+"="+env[runtimeLaunchIDEnv])
		hostCommand.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
		stdout, err := hostCommand.StdoutPipe()
		if err != nil {
			return "", 0, err
		}
		hostCommand.Stderr = stderr
		if err = hostCommand.Start(); err != nil {
			return "", 0, err
		}
		t.Cleanup(func() { _ = hostCommand.Process.Kill() })
		go func() { _ = hostCommand.Wait() }()
		ready := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 4096), 4096)
			if scanner.Scan() {
				ready <- scanner.Text()
			} else {
				ready <- ""
			}
		}()
		select {
		case line := <-ready:
			var port int
			if _, err := fmt.Sscanf(line, "READY:%d %d", &helperPID, &port); err != nil || helperPID <= 0 || port <= 0 || port > 65535 {
				return "", hostCommand.Process.Pid, fmt.Errorf("invalid owned READY: %q stderr=%s", line, stderr.String())
			}
			_ = stdout.Close()
			address = fmt.Sprintf("127.0.0.1:%d", port)
			return address, hostCommand.Process.Pid, nil
		case <-ctx.Done():
			return "", hostCommand.Process.Pid, ctx.Err()
		}
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	handle, err := rt.Create(ctx, ports.RuntimeConfig{SessionID: domain.SessionID(id), WorkspacePath: root, Argv: []string{c.Helper}, Env: map[string]string{runtimeLaunchIDEnv: generation}})
	if err != nil {
		t.Fatalf("candidate ConPTY launch failed: %v stderr=%s", err, stderr.String())
	}
	host := candidateProcess(t, candidateFixtureIdentity{PID: hostCommand.Process.Pid}, c.Executable)
	child := candidateProcess(t, candidateFixtureIdentity{PID: helperPID}, c.Helper)
	t.Cleanup(func() {
		if err := child.stop(); err != nil {
			t.Errorf("owned helper cleanup: %v", err)
		}
		child.close()
		if err := host.stop(); err != nil {
			t.Errorf("owned host cleanup: %v", err)
		}
		host.close()
	})
	candidateWait(t, 5*time.Second, "stdlib helper readiness", func() bool {
		b, e := os.ReadFile(filepath.Join(root, "worker.ready"))
		return e == nil && string(b) == nonce
	})
	status, alive, err := clientStatusContext(t.Context(), address)
	if err != nil || !alive || !status.Alive || status.PID != helperPID || status.HostIdentity.SessionID != id || status.HostIdentity.RuntimeLaunchID != generation || status.HostIdentity.LaunchID == "" || status.HostIdentity.CreatedAt == "" || status.HostIdentity.PID != hostCommand.Process.Pid {
		t.Fatalf("initial child status mismatch: %+v alive=%t err=%v", status, alive, err)
	}
	return rt, handle, candidateFixtureHandoff{Session: id, Generation: generation, Nonce: nonce, Host: host.identity, Helper: child.identity, Address: address}, host, child
}

func candidateState(t *testing.T) (string, string) {
	t.Helper()
	nonce := strings.ReplaceAll(uuid.NewString(), "-", "")
	root := filepath.Join(t.TempDir(), "ao-conpty-fixture-"+nonce)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "owner-token"), []byte(nonce), 0600); err != nil {
		t.Fatal(err)
	}
	return root, nonce
}

// The parent deliberately terminates this owned controller test process. Only
// the explicit helper is placed under the actual candidate's detached host.
func TestCandidateConPTYOwnerProcess(t *testing.T) {
	if os.Getenv("AO_CONPTY_FIXTURE_OWNER") != "1" {
		t.Skip("owned fixture subprocess only")
	}
	c := candidateConfig(t)
	root, nonce := os.Getenv("AO_CONPTY_FIXTURE_ROOT"), os.Getenv("AO_CONPTY_FIXTURE_NONCE")
	if !filepath.IsAbs(root) || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(nonce) || filepath.Base(root) != "ao-conpty-fixture-"+nonce {
		t.Fatal("invalid isolated owner identity")
	}
	marker, err := os.ReadFile(filepath.Join(root, "owner-token"))
	if err != nil || string(marker) != nonce {
		t.Fatal("owner marker mismatch")
	}
	_, _, record, _, _ := candidateCreate(t, c, root, nonce)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, "handoff.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("owner fixture deadline")
	}
}

func TestActualCandidateConPTYMechanics(t *testing.T) {
	c := candidateConfig(t)
	if os.Getenv("AO_CONPTY_FIXTURE_OWNER") == "1" {
		t.Fatal("controller may not recurse into parent fixture")
	}
	t.Cleanup(func() { ptyregistry.SetRunFilePath("") })
	root, nonce := candidateState(t)
	dirtyPath := filepath.Join(root, "dirty-work.txt")
	if err := os.WriteFile(dirtyPath, []byte("preserved uncommitted fixture contribution\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dirtyBefore, err := candidateHashFile(dirtyPath)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(self, "-test.run=^TestCandidateConPTYOwnerProcess$", "-test.v", "-test.timeout=75s")
	owner.Env = candidateEnvironment(c, root, nonce, true)
	owner.Dir = root
	owner.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	ownerOutput := &boundedBuffer{max: 32 << 10}
	owner.Stdout = ownerOutput
	owner.Stderr = ownerOutput
	stdin, err := owner.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- owner.Wait() }()
	t.Cleanup(func() { _ = stdin.Close(); _ = owner.Process.Kill() })
	var record candidateFixtureHandoff
	candidateWait(t, 20*time.Second, "owned controller handoff", func() bool {
		b, e := os.ReadFile(filepath.Join(root, "handoff.json"))
		if e == nil && json.Unmarshal(b, &record) == nil {
			return true
		}
		select {
		case e := <-ownerDone:
			t.Fatalf("controller exited before handoff: %v %s", e, ownerOutput.String())
		default:
		}
		return false
	})
	if record.Nonce != nonce || record.Session != "offline-"+nonce || record.Generation != "generation-"+nonce {
		t.Fatal("controller handoff identity mismatch")
	}
	host := candidateProcess(t, record.Host, c.Executable)
	child := candidateProcess(t, record.Helper, c.Helper)
	t.Cleanup(func() { _ = child.stop(); child.close(); _ = host.stop(); host.close() })
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ownerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("owned controller did not exit")
	}
	if live, err := host.alive(); err != nil || !live {
		t.Fatalf("detached candidate host did not survive controller exit: %t %v", live, err)
	}
	if live, err := child.alive(); err != nil || !live {
		t.Fatalf("helper did not survive controller exit: %t %v", live, err)
	}
	rt := New(Options{RunFilePath: filepath.Join(root, "running.json"), Spawner: func(context.Context, string, string, []string, map[string]string, HostOptions) (string, int, error) {
		return "", 0, errors.New("recovery must adopt, never launch")
	}})
	handle := ports.RuntimeHandle{ID: record.Session}
	ref := ports.FencedRuntimeRef{Handle: handle, SessionID: domain.SessionID(record.Session), Generation: record.Generation}
	if got := rt.ProbeFencedRuntime(t.Context(), ref); got.Liveness != ports.FencedAlive {
		t.Fatalf("reconstructed runtime did not adopt exact host: %+v", got)
	}
	wrong := ref
	wrong.Generation = "stale-generation"
	if got := rt.ProbeFencedRuntime(t.Context(), wrong); got.Liveness != ports.FencedUnknown || got.Reason != ports.FencedReasonGenerationMismatch {
		t.Fatalf("stale generation accepted: %+v", got)
	}
	for i := 0; i < 3; i++ {
		stream, err := rt.Attach(t.Context(), handle, 24, 80)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write([]byte("OFFLINE-FIXTURE-NO-COMMAND\r\n")); err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		if live, err := rt.IsChildAlive(t.Context(), handle); err != nil || !live {
			t.Fatalf("detach killed helper: %t %v", live, err)
		}
	}
	if err := child.stop(); err != nil {
		t.Fatal(err)
	}
	candidateWait(t, 5*time.Second, "explicit child-exit status", func() bool {
		s, h, e := clientStatusContext(t.Context(), record.Address)
		return e == nil && h && !s.Alive && s.ExitCode != nil
	})
	if live, err := rt.IsAlive(t.Context(), handle); err != nil || !live {
		t.Fatalf("host status conflated child exit with host death: %t %v", live, err)
	}
	if got := rt.ProbeFencedRuntime(t.Context(), ref); got.Liveness != ports.FencedDead {
		t.Fatalf("exited child reported alive: %+v", got)
	}
	if err := host.stop(); err != nil {
		t.Fatal(err)
	}
	if err := ptyregistry.Unregister(t.Context(), record.Session); err != nil {
		t.Fatal(err)
	}
	dirtyAfter, err := candidateHashFile(dirtyPath)
	if err != nil || dirtyBefore != dirtyAfter {
		t.Fatal("candidate recovery changed dirty fixture content")
	}
	t.Logf("CANDIDATE_CONPTY_RECOVERY candidateSource=%s candidateSHA256=%s helperSHA256=%s ownerPID=%d hostPID=%d helperPID=%d reconnects=3 dirtySHA256=%s providerCalls=0 fullDaemonRun=false", c.SourceCommit, c.ExecutableSHA256, c.HelperSHA256, owner.Process.Pid, record.Host.PID, record.Helper.PID, dirtyAfter)
	crashRoot, crashNonce := candidateState(t)
	_, _, crash, crashHost, crashChild := candidateCreate(t, c, crashRoot, crashNonce)
	if err := crashHost.stop(); err != nil {
		t.Fatal(err)
	}
	candidateWait(t, 5*time.Second, "helper exit after owned host crash", func() bool { live, e := crashChild.alive(); return e == nil && !live })
	if err := ptyregistry.Unregister(t.Context(), crash.Session); err != nil {
		t.Fatal(err)
	}
	t.Logf("CANDIDATE_CONPTY_HOST_CRASH candidateSource=%s hostPID=%d helperPID=%d directHelperExitObserved=true descendantCoverage=UNKNOWN providerCalls=0", c.SourceCommit, crash.Host.PID, crash.Helper.PID)
}
