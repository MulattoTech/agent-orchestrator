//go:build windows

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerkeys"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"golang.org/x/sys/windows"
)

// Linker-controlled test-binary entry only; no normal AO mode selects fakes.
var OfflineDaemonFixture string
var OfflineDaemonFixtureSource string

const fixtureRootPurpose = "AO MTechLabs offline daemon fixture"

type daemonFixtureMarker struct {
	Purpose       string    `json:"purpose"`
	RunID         string    `json:"runId"`
	OwnerSID      string    `json:"ownerSID"`
	SourceCommit  string    `json:"sourceCommit"`
	FixtureSHA256 string    `json:"fixtureSHA256"`
	CreatedAtUTC  time.Time `json:"createdAtUtc"`
}
type daemonFixtureChildMarker struct {
	Phase     string `json:"phase"`
	Nonce     string `json:"nonce"`
	ParentPID int    `json:"parentPid"`
}

func TestMain(m *testing.M) {
	if OfflineDaemonFixture == "1" {
		if len(os.Args) != 1 {
			fmt.Fprintln(os.Stderr, "packaged daemon fixture accepts no arguments")
			os.Exit(2)
		}
		selected := "^TestOfflineDaemonLifecycleRecovery$"
		if os.Getenv("AO_DAEMON_FIXTURE_CHILD") != "" {
			selected = "^TestOfflineDaemonLifecycleChild$"
		}
		os.Args = []string{os.Args[0], "-test.run=" + selected, "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=180s"}
	}
	os.Exit(m.Run())
}
func fixtureHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func readFixtureJSON(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 2<<20))
	dec.DisallowUnknownFields()
	if err = dec.Decode(target); err != nil {
		return err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return errors.New("fixture JSON has trailing content")
	}
	return nil
}
func writeFixtureJSONNew(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}
func fixtureCurrentSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}
func fixturePrivateRoot(path, sid string) error {
	if !regexp.MustCompile(`(?i)^C:\\AO-MTechLabs-DaemonFixture-[0-9a-f]{32}$`).MatchString(path) {
		return errors.New("fixture root is not a fresh reserved C-drive child")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture root is not a physical directory")
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.EqualFold(path, physical) {
		return errors.New("fixture root has a reparse ancestor")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != sid {
		return errors.New("fixture root owner differs from process owner")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("fixture root lacks a DACL")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("fixture root DACL is not protected")
	}
	// Root runner created exactly trusted allow entries. Unknown ACE types and
	// principals fail closed; inherited-only entries cannot grant this root.
	type aclHeader struct {
		Revision byte
		Sbz1     byte
		Size     uint16
		Count    uint16
		Sbz2     uint16
	}
	header := (*aclHeader)(unsafe.Pointer(acl))
	for i := uint32(0); i < uint32(header.Count); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, i, &ace); err != nil || ace == nil {
			return errors.New("fixture DACL unreadable")
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("fixture DACL has unsupported ACE")
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !principal.IsValid() {
			return errors.New("fixture DACL has invalid SID")
		}
		v := principal.String()
		if v != sid && v != "S-1-5-18" && v != "S-1-5-32-544" {
			return errors.New("fixture DACL grants an untrusted principal")
		}
	}
	return nil
}
func validateDaemonFixture(t *testing.T) (string, daemonFixtureMarker, string) {
	t.Helper()
	if OfflineDaemonFixture != "1" {
		t.Skip("requires fixed packaged offline daemon companion")
	}
	root, source, expected := os.Getenv("AO_DAEMON_FIXTURE_ROOT"), os.Getenv("AO_DAEMON_FIXTURE_SOURCE"), os.Getenv("AO_DAEMON_FIXTURE_EXE_SHA256")
	if !regexp.MustCompile("^[0-9a-f]{40}$").MatchString(source) || source != OfflineDaemonFixtureSource {
		t.Fatal("fixture source is not linker-pinned")
	}
	if !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(expected) {
		t.Fatal("fixture hash invalid")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(executable) != "ao-offline-daemon.exe" || !strings.HasPrefix(strings.ToLower(executable), `c:\aodev\releases\ao-unofficial-`) {
		t.Fatal("fixture executable outside candidate bundle")
	}
	actual, err := fixtureHashFile(executable)
	if err != nil || actual != expected {
		t.Fatal("fixture executable hash mismatch")
	}
	var marker daemonFixtureMarker
	if err = readFixtureJSON(filepath.Join(root, "fixture-root.json"), &marker); err != nil {
		t.Fatal(err)
	}
	sid, err := fixtureCurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixturePrivateRoot(root, sid); err != nil {
		t.Fatal(err)
	}
	if marker.Purpose != fixtureRootPurpose || marker.OwnerSID != sid || marker.SourceCommit != source || marker.FixtureSHA256 != actual || marker.RunID == "" || marker.CreatedAtUTC.IsZero() {
		t.Fatal("fixture root marker mismatch")
	}
	manifestPath := filepath.Join(filepath.Dir(executable), "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	hashBytes, err := os.ReadFile(filepath.Join(filepath.Dir(executable), "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(digest[:]) != strings.TrimSpace(string(hashBytes)) {
		t.Fatal("candidate manifest hash mismatch")
	}
	var manifest struct {
		SourceCommit string `json:"sourceCommit"`
		StrictPolicy bool   `json:"strictPolicy"`
		Files        map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.SourceCommit != source || !manifest.StrictPolicy || manifest.Files["ao-offline-daemon.exe"].SHA256 != actual {
		t.Fatal("fixture not represented by candidate manifest")
	}
	if os.Getenv("AO_LOCAL_LIFECYCLE_TEST") != "" {
		t.Fatal("partial lifecycle bypass is forbidden")
	}
	return root, marker, executable
}
func fixturePolicy(root string) inferenceguard.Policy {
	roles := map[string]inferenceguard.RolePolicy{}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		roles[role] = inferenceguard.RolePolicy{DefaultModel: fixtureAgentModel, Models: []string{fixtureAgentModel}}
	}
	return inferenceguard.Policy{Version: 1, AccountKey: "synthetic-fixture-account", AuthorizationEpoch: "synthetic-fixture-epoch", NativeAccountSHA256: inferenceguard.AccountFingerprint("synthetic", "fixture.invalid"), AuthorityPath: filepath.Join(root, "authority", "inference.sqlite"), EmergencyStop: true, Pause: true, MaximumEffort: "medium", MaximumInference: 1, MaximumPerProject: 1, MaximumReviewers: 1, TurnBudget: 3, ReserveTurns: 1, Roles: roles}
}
func prepareFixtureDirectories(t *testing.T, root string) {
	t.Helper()
	for _, part := range []string{"state", "workspaces", "synthetic-writes", "tmp", "import-home", "legacy-import", "authority"} {
		if _, err := os.Lstat(filepath.Join(root, part)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fixture refuses existing state directory")
		}
		if err := os.MkdirAll(filepath.Join(root, part), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFixtureJSONNew(filepath.Join(root, "policy.json"), fixturePolicy(root)); err != nil {
		t.Fatal(err)
	}
}

type fixtureSeedState struct {
	DirtyPath          string
	DirtyBytes         []byte
	ReportContent      string
	ReportConversation string
	ReportBatch        string
	QueueTurns         []string
}

func seedDaemonFixture(t *testing.T, root string) fixtureSeedState {
	t.Helper()
	ctx := context.Background()
	effects := &fixtureEffectRecorder{}
	workspace := fixtureWorkspace(filepath.Join(root, "workspaces"), effects)
	store, err := sqlite.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now().UTC()
	// Enable the ordinary local account-limit poller over the explicit fake
	// provider. This does not enable the production allowance-check bridge.
	if err = store.SetLimitHistory(ctx, true, now); err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertProject(ctx, domain.ProjectRecord{ID: "fixture-project", Kind: domain.ProjectKindScratch, Path: filepath.Join(root, "workspaces"), DisplayName: "Synthetic recovery fleet", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	var state fixtureSeedState
	for i := 0; i < 40; i++ {
		id := domain.SessionID(fmt.Sprintf("fixture-worker-%02d", i))
		kind := domain.KindWorker
		scope := domain.ConversationScopeSession
		if i == 0 {
			id = "fixture-orchestrator"
			kind = domain.KindOrchestrator
			scope = domain.ConversationScopeProject
		}
		info, err := workspace.Create(ctx, ports.WorkspaceConfig{ProjectID: "fixture-project", SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		generation := "seed-generation-" + string(id)
		_, err = store.CreateSession(ctx, domain.SessionRecord{ID: id, ProjectID: "fixture-project", Kind: kind, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{WorkspacePath: info.Path, ProviderConversationID: "synthetic-thread-" + string(id), ControllerGeneration: generation, Model: fixtureAgentModel, Effort: fixtureAgentEffort}})
		if err != nil {
			t.Fatal(err)
		}
		conversation, err := store.CreateConversation(ctx, "conversation-"+string(id), scope, "fixture-project", id, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ClaimChatControllerGeneration(ctx, id, generation); err != nil {
			t.Fatal(err)
		}
		if i == 1 || i == 2 {
			continue
		}
		msg := domain.ConversationMessage{ID: "message-" + string(id), Text: "Synthetic held request", Origin: domain.MessageOriginHuman, ClientMessageID: "queued-" + string(id)}
		if i == 0 {
			if _, err = store.CreateReport(ctx, domain.ReportRecord{ID: "fixture-report", SessionID: "fixture-orchestrator", ProjectID: "fixture-project", State: domain.ReportCheckpoint, Message: "Synthetic durable report", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimPendingReportBatch(ctx, "fixture-project", "crashed-claimant", now)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim=%v,%v", claimed, err)
			}
			state.ReportBatch = claimed[0].DeliveryBatchID
			content, err := json.Marshal([]ports.ChatContent{{Type: "text", Internal: true, URI: ports.ChatWorkerReportURIPrefix + state.ReportBatch, Text: "Synthetic durable report"}})
			if err != nil {
				t.Fatal(err)
			}
			msg.DeliveryContentJSON = string(content)
			state.ReportContent = string(content)
			state.ReportConversation = conversation.ID
			state.DirtyPath = filepath.Join(info.Path, "uncommitted-fixture.txt")
			state.DirtyBytes = []byte("dirty synthetic work must survive all daemon episodes\r\n")
			if err = os.WriteFile(state.DirtyPath, state.DirtyBytes, 0600); err != nil {
				t.Fatal(err)
			}
		}
		turn := "queued-turn-" + string(id)
		made, err := store.AppendUserMessage(ctx, conversation.ID, id, generation, msg, turn, now)
		if err != nil || !made {
			t.Fatalf("seed turn=%v,%v", made, err)
		}
		state.QueueTurns = append(state.QueueTurns, turn)
	}
	request := domain.SessionAdmissionRequest{ID: "fixture-uncertain-admission", ProjectID: "fixture-project", SessionID: "fixture-worker-03", Operation: "restore", Payload: `{"sessionId":"fixture-worker-03"}`, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	if err = store.EnqueueSessionAdmission(ctx, request); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimSessionAdmission(ctx, "interrupted-owner", now)
	if err != nil || !ok {
		t.Fatalf("admission claim=%v,%v", ok, err)
	}
	claimed.State = "blocked"
	claimed.FailureCode = "LAUNCH_OUTCOME_UNCERTAIN"
	claimed.UpdatedAt = now
	if ok, err = store.CompleteSessionAdmission(ctx, claimed); err != nil || !ok {
		t.Fatalf("admission quarantine=%v,%v", ok, err)
	}
	for _, entry := range effects.entries {
		if entry.Kind == "forbidden" {
			t.Fatalf("forbidden seed effect %s", entry.Name)
		}
	}
	return state
}

type daemonFixtureProcess struct {
	exited      atomic.Bool
	cmd         *exec.Cmd
	done        chan error
	log         *os.File
	root, phase string
	address     string
	runID       string
}

func fixtureChildEnv(root, source, hash, phase, nonce string) []string {
	systemRoot := os.Getenv("SystemRoot")
	return []string{
		"SystemRoot=" + systemRoot, "WINDIR=" + systemRoot, "SystemDrive=C:", "PATH=" + filepath.Join(systemRoot, "System32"), "PATHEXT=.COM;.EXE;.BAT;.CMD", "COMSPEC=" + filepath.Join(systemRoot, "System32", "cmd.exe"),
		"TEMP=" + filepath.Join(root, "tmp"), "TMP=" + filepath.Join(root, "tmp"), "GOMAXPROCS=2",
		"AO_DAEMON_FIXTURE_ROOT=" + root, "AO_DAEMON_FIXTURE_SOURCE=" + source, "AO_DAEMON_FIXTURE_EXE_SHA256=" + hash,
		"AO_DAEMON_FIXTURE_CHILD=" + phase, "AO_DAEMON_FIXTURE_NONCE=" + nonce,
		"AO_ENABLE_ALLOWANCE_CHECK=0", "AO_REQUIRE_INFERENCE_POLICY=1",
	}
}
func launchDaemonFixtureChild(t *testing.T, root, exe string, marker daemonFixtureMarker, phase string) *daemonFixtureProcess {
	t.Helper()
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	childMarker := daemonFixtureChildMarker{Phase: phase, Nonce: nonce, ParentPID: os.Getpid()}
	if err := writeFixtureJSONNew(filepath.Join(root, phase+"-child.json"), childMarker); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(root, phase+"-child.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Dir = root
	cmd.Env = fixtureChildEnv(root, marker.SourceCommit, marker.FixtureSHA256, phase, nonce)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	child := &daemonFixtureProcess{cmd: cmd, done: make(chan error, 1), log: log, root: root, phase: phase, runID: marker.RunID}
	go func() { err := cmd.Wait(); child.exited.Store(true); child.done <- err }()
	t.Cleanup(func() {
		if !child.exited.Load() {
			_ = cmd.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(10 * time.Second):
				t.Error("owned fixture child did not exit")
			}
		}
		_ = log.Close()
	})
	return child
}
func readFixtureEffects(path string) ([]fixtureEffectEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []fixtureEffectEntry{}
	scanner := bufio.NewScanner(io.LimitReader(f, 16<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var e fixtureEffectEntry
		if err = json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}
func fixtureWait(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
func (c *daemonFixtureProcess) waitReady(t *testing.T) {
	t.Helper()
	fixtureWait(t, c.phase+" HTTP and recovery", func() bool {
		entries, err := readFixtureEffects(filepath.Join(c.root, c.phase+"-effects.jsonl"))
		if err != nil {
			return false
		}
		ready, recovered := false, false
		observed := map[string]int{}
		for _, e := range entries {
			if e.Kind == "allowed" {
				observed[e.Name]++
			}
			if e.Kind == "forbidden" {
				t.Fatalf("forbidden boot effect %s", e.Name)
			}
			if e.Name == "http.ready" {
				c.address = e.Address
				ready = true
			}
			if e.Name == "recovery.finished" {
				recovered = true
			}
		}
		if c.phase == "crash" {
			for _, effect := range []string{"model.discover", "agent.readiness.binary", "agent.readiness.authentication", "account.capabilities", "account.provider.limits"} {
				if observed[effect] == 0 {
					return false
				}
			}
		}
		return ready && recovered
	})
	var info runfile.Info
	if err := readFixtureJSON(filepath.Join(c.root, "state", "running.json"), &info); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(c.address)
	if err != nil || host != "127.0.0.1" || port == "39421" || port == "39431" || info.PID != c.cmd.Process.Pid || info.AppRunID != c.runID || strconv.Itoa(info.Port) != port {
		t.Fatal("fixture runfile/listener ownership mismatch")
	}
}
func (c *daemonFixtureProcess) request(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	allowed := map[string]bool{"GET /healthz": true, "GET /api/v1/sessions": true, "GET /internal/reliability": true, "PUT /internal/reliability/inference": true, "POST /shutdown": true}
	for _, id := range []string{"fixture-worker-01", "fixture-worker-02"} {
		allowed["POST /api/v1/sessions/"+id+"/conversation/messages"] = true
		allowed["GET /api/v1/sessions/"+id+"/conversation"] = true
	}
	if !allowed[method+" "+path] {
		t.Fatal("fixture HTTP request outside fixed allowlist")
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != c.address {
			return nil, errors.New("fixture refuses foreign HTTP address")
		}
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture refuses redirects") }}
	req, err := http.NewRequest(method, "http://"+c.address+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, response.StatusCode, want, result)
	}
	return result
}
func (c *daemonFixtureProcess) waitExit(t *testing.T, wantSuccess bool) {
	t.Helper()
	select {
	case err := <-c.done:
		if (err == nil) != wantSuccess {
			t.Fatalf("child exit=%v, success wanted=%v", err, wantSuccess)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("owned child did not exit")
	}
	if err := c.log.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestOfflineDaemonLifecycleChild(t *testing.T) {
	root, marker, _ := validateDaemonFixture(t)
	phase := os.Getenv("AO_DAEMON_FIXTURE_CHILD")
	if phase != "failure" && phase != "crash" && phase != "recovery" {
		t.Fatal("invalid child phase")
	}
	var child daemonFixtureChildMarker
	if err := readFixtureJSON(filepath.Join(root, phase+"-child.json"), &child); err != nil {
		t.Fatal(err)
	}
	if child.Phase != phase || child.Nonce != os.Getenv("AO_DAEMON_FIXTURE_NONCE") || !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(child.Nonce) || child.ParentPID != os.Getppid() {
		t.Fatal("child ownership nonce/parent mismatch")
	}
	stateRoot := root
	if phase == "failure" {
		stateRoot = filepath.Join(root, "startup-failure")
	}
	policyPath := filepath.Join(stateRoot, "policy.json")
	if err := os.Setenv(inferenceguard.PolicyEnv, policyPath); err != nil {
		t.Fatal(err)
	}
	effects, err := newFixtureEffectRecorder(filepath.Join(root, phase+"-effects.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := effects.Close(); err != nil {
			t.Error(err)
		}
	}()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = fixtureDeniedTransport{effects}
	defer func() { http.DefaultTransport = originalTransport }()
	for _, file := range []string{"provider-api-keys.json", "provider-api-keys-device.json", "provider-api-keys-device-user.json"} {
		if _, err = os.Lstat(filepath.Join(stateRoot, "state", file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fixture refuses existing provider-key state")
		}
	}
	if keys, err := providerkeys.Load(filepath.Join(stateRoot, "state")); err != nil || len(keys) != 0 {
		t.Fatal("fixture provider-key state not empty")
	}
	deps := fixtureBootDependencies(stateRoot, marker.RunID, phase, effects)
	if phase == "failure" {
		deps.Workspace = func(config.Config, *sqlite.Store, *slog.Logger) (ports.Workspace, error) {
			return nil, errors.New("injected workspace construction failure")
		}
	}
	err = runWithDependencies("offline-daemon-fixture", deps)
	if phase == "failure" {
		if err == nil || !strings.Contains(err.Error(), "injected workspace construction failure") {
			t.Fatalf("startup failure=%v", err)
		}
		lock, err := datadirlock.Acquire(context.Background(), filepath.Join(stateRoot, "state"), 0)
		if err != nil {
			t.Fatalf("startup cleanup retained data lock: %v", err)
		}
		store, err := sqlite.Open(filepath.Join(stateRoot, "state"))
		if err != nil {
			lock.Release()
			t.Fatal(err)
		}
		if _, err = store.ListAllSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		if err = lock.Release(); err != nil {
			t.Fatal(err)
		}
		effects.Allow("failure.same-process-store-reopen")
	} else if err != nil {
		t.Fatal(err)
	}
	if gate, err := inferenceguard.ForDataDir(filepath.Join(stateRoot, "state")); err == nil && gate != nil {
		if err = gate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range effects.entries {
		if entry.Kind == "forbidden" {
			t.Fatalf("forbidden effect %s", entry.Name)
		}
	}
	effects.Allow("child.returned")
}
func TestOfflineDaemonLifecycleRecovery(t *testing.T) {
	root, marker, exe := validateDaemonFixture(t)
	if os.Getenv("AO_DAEMON_FIXTURE_CHILD") != "" {
		t.Fatal("parent fixture cannot run in child mode")
	}
	prepareFixtureDirectories(t, root)
	prepareFixtureDirectories(t, filepath.Join(root, "startup-failure"))
	seed := seedDaemonFixture(t, root)
	fail := launchDaemonFixtureChild(t, root, exe, marker, "failure")
	fail.waitExit(t, true)
	first := launchDaemonFixtureChild(t, root, exe, marker, "crash")
	first.waitReady(t)
	first.request(t, "GET", "/healthz", nil, http.StatusOK)
	first.request(t, "GET", "/api/v1/sessions", nil, http.StatusOK)
	first.request(t, "GET", "/internal/reliability", nil, http.StatusOK)
	first.request(t, "PUT", "/internal/reliability/inference", map[string]any{"emergency_stop": false, "pause": false}, http.StatusNoContent)
	first.request(t, "POST", "/api/v1/sessions/fixture-worker-01/conversation/messages", map[string]string{"text": "Complete one synthetic turn", "clientMessageId": "fixture-complete"}, http.StatusAccepted)
	fixtureWait(t, "completed synthetic HTTP turn", func() bool {
		return bytes.Contains(first.request(t, "GET", "/api/v1/sessions/fixture-worker-01/conversation", nil, http.StatusOK), []byte("Synthetic completion; no model executed."))
	})
	first.request(t, "POST", "/api/v1/sessions/fixture-worker-02/conversation/messages", map[string]string{"text": "Hold one synthetic turn across crash", "clientMessageId": "fixture-uncertain"}, http.StatusAccepted)
	fixtureWait(t, "durable synthetic second write", func() bool {
		_, err := os.Stat(filepath.Join(root, "synthetic-writes", "fixture-uncertain.json"))
		return err == nil
	})
	// Persist pause without attempting a provider interrupt, then terminate only
	// the child whose process handle this parent retained from Start.
	first.request(t, "PUT", "/internal/reliability/inference", map[string]any{"pause": true}, http.StatusNoContent)
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.waitExit(t, false)
	second := launchDaemonFixtureChild(t, root, exe, marker, "recovery")
	second.waitReady(t)
	second.request(t, "GET", "/healthz", nil, http.StatusOK)
	second.request(t, "GET", "/api/v1/sessions", nil, http.StatusOK)
	account := second.request(t, "GET", "/internal/reliability", nil, http.StatusOK)
	if !bytes.Contains(account, []byte(`"uncertain":1`)) {
		t.Fatalf("recovery did not retain uncertain authority: %s", account)
	}
	second.request(t, "POST", "/shutdown", map[string]any{"stopAgents": false, "appRunId": marker.RunID}, http.StatusAccepted)
	second.waitExit(t, true)
	transport := &net.Dialer{Timeout: time.Second}
	conn, err := transport.Dial("tcp", second.address)
	if err == nil {
		conn.Close()
		t.Fatal("graceful shutdown retained HTTP listener")
	}
	if _, err = os.Stat(filepath.Join(root, "state", "running.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("graceful shutdown retained runfile")
	}
	store, err := sqlite.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions, err := store.ListAllSessions(ctx)
	if err != nil || len(sessions) != 40 {
		t.Fatalf("registered sessions=%d err=%v", len(sessions), err)
	}
	for _, id := range seed.QueueTurns {
		turn, err := store.TurnByID(ctx, id)
		if err != nil || turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" {
			t.Fatalf("held queue %s=%+v,%v", id, turn, err)
		}
	}
	report, found, err := store.GetReport(ctx, "fixture-report")
	if err != nil || !found || report.DeliveryState != domain.ReportAcknowledged || report.DeliveryBatchID != seed.ReportBatch || report.DeliveryAttempts != 1 {
		t.Fatalf("report recovery=%+v,%v", report, err)
	}
	message, found, err := store.ConversationMessageByClientID(ctx, seed.ReportConversation, "queued-fixture-orchestrator")
	if err != nil || !found || message.DeliveryContentJSON != seed.ReportContent {
		t.Fatalf("durable report attachment changed: found=%v err=%v", found, err)
	}
	messages, err := store.ConversationMessages(ctx, seed.ReportConversation)
	if err != nil {
		t.Fatal(err)
	}
	attachments := 0
	for _, message := range messages {
		var content []ports.ChatContent
		if message.DeliveryContentJSON != "" {
			if err = json.Unmarshal([]byte(message.DeliveryContentJSON), &content); err != nil {
				t.Fatal(err)
			}
		}
		for _, item := range content {
			if item.URI == ports.ChatWorkerReportURIPrefix+seed.ReportBatch {
				attachments++
				if !item.Internal || item.Text != "Synthetic durable report" {
					t.Fatal("report attachment mutated")
				}
			}
		}
	}
	if attachments != 1 {
		t.Fatalf("report attached %d times", attachments)
	}
	rows, err := store.ListSessionAdmission(ctx)
	if err != nil || len(rows) != 1 || rows[0].State != "blocked" || rows[0].FailureCode != "LAUNCH_OUTCOME_UNCERTAIN" {
		t.Fatalf("uncertain admission=%+v,%v", rows, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	dirty, err := os.ReadFile(seed.DirtyPath)
	if err != nil || !bytes.Equal(dirty, seed.DirtyBytes) {
		t.Fatal("dirty scratch work changed")
	}
	writes, err := os.ReadDir(filepath.Join(root, "synthetic-writes"))
	if err != nil || len(writes) != 2 {
		t.Fatalf("synthetic write count=%d err=%v", len(writes), err)
	}
	allowed, forbidden, starts, closed := 0, 0, 0, 0
	allowedCounts := map[string]int{}
	failureReopen := false
	for _, phase := range []string{"failure", "crash", "recovery"} {
		entries, err := readFixtureEffects(filepath.Join(root, phase+"-effects.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			switch entry.Kind {
			case "allowed":
				allowed++
				allowedCounts[entry.Name]++
			case "forbidden":
				forbidden++
			}
			if entry.Name == "http.ready" {
				starts++
			}
			if entry.Name == "store.closed" {
				closed++
			}
			if entry.Name == "failure.same-process-store-reopen" {
				failureReopen = true
			}
			if entry.Name == "cleanup.failed" || entry.Name == "store.close-failed" {
				t.Fatal("incomplete cleanup observed")
			}
		}
	}
	if allowedCounts["chat.synthetic-provider-write"] != 2 {
		t.Fatalf("synthetic write callbacks=%d", allowedCounts["chat.synthetic-provider-write"])
	}
	if forbidden != 0 || starts != 2 || closed != 2 || !failureReopen {
		t.Fatalf("effects forbidden=%d starts=%d closed=%d failureReopen=%v", forbidden, starts, closed, failureReopen)
	}
	proof := map[string]any{
		"sourceCommit": marker.SourceCommit, "fixtureSHA256": marker.FixtureSHA256, "runStarts": starts, "registeredSessions": len(sessions), "forbiddenEffectCount": forbidden, "realProviderCalls": 0,
		"crashRecoveryVerified": true, "ownedChildCrash": true, "gracefulShutdownVerified": true, "dirtyFilePreserved": true, "startupFailureCleanupVerified": true, "uncertainDispatchQuarantined": true, "uncertainAdmissionPreserved": true,
		"queuedRequestsPreserved": len(seed.QueueTurns), "durableReportAttachmentPreserved": true, "syntheticProviderWrites": len(writes), "allowedSyntheticEffects": allowed, "allowedSyntheticEffectCounts": allowedCounts,
		"scope": "Shared daemon Run lifecycle; real SQLite, locks, migrations, CDC, HTTP, queues and recovery; explicit synthetic provider/runtime/account/SCM leaves. No real native or inference qualification.",
	}
	if err = writeFixtureJSONNew(filepath.Join(root, "fixture-proof.json"), proof); err != nil {
		t.Fatal(err)
	}
}
