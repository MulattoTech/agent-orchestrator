package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/runtimeselect"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// This contract is implemented by the parent subprocess fixture's durable
// effect recorder. These stand-ins have no native adapter or process fallback.
type bootFixtureEffects interface {
	Allow(string)
	Deny(string) error
}

const fixtureAgentModel = "fixture-model"
const fixtureAgentEffort = "medium"

func fixtureDenied(effects bootFixtureEffects, operation string) error {
	if bootAgentDependencyNil(effects) {
		return fmt.Errorf("fixture has no effect recorder: %s", operation)
	}
	if err := effects.Deny(operation); err != nil {
		return err
	}
	return fmt.Errorf("fixture operation denied: %s", operation)
}

func fixtureAgentComponents(ctx context.Context, cfg config.Config, store *sqlite.Store, gate ports.CodexOperationGate, log *slog.Logger, effects bootFixtureEffects) (agentBootComponents, error) {
	if bootAgentDependencyNil(effects) {
		return agentBootComponents{}, errors.New("fixture effects are required")
	}
	home := filepath.Join(cfg.StateDir, "fixture-codex-home")
	factory := fixtureAccountFactory{effects: effects, stateRoot: cfg.StateDir}
	return newExplicitAgentBoot(ctx, cfg, store, gate, log, explicitAgentBootInputs{
		Harnesses: func() []agentregistry.HarnessAgent {
			effects.Allow("agent.registry")
			return []agentregistry.HarnessAgent{{
				Harness:  domain.HarnessCodex,
				Manifest: adapters.Manifest{ID: string(domain.HarnessCodex), Name: "Synthetic fixture agent"},
				Agent:    fixtureCatalogAgent{effects: effects},
			}}
		},
		Discoverer: fixtureModels{effects: effects},
		ModelGate: func(_ context.Context, _ string, catalog ports.AgentModelCatalog) ports.AgentModelCatalog {
			effects.Allow("model.gate")
			return catalog
		},
		CodexHome: home, CodexAccounts: factory,
		Providers:        []ports.AgentAccountProviderCLI{fixtureAccountProvider{effects: effects, stateRoot: cfg.StateDir}},
		AccountTerminals: fixtureAccountTerminals{effects: effects},
		// Explicit synthetic provider configuration exercises AuthStatus through
		// the real readiness coordinator. No native credential file is created
		// or copied. WarmCodexAccounts still validates the private empty home.
		UsesCustomCodexModelProvider: func() bool {
			effects.Allow("account.synthetic-provider-config")
			return true
		},
		ModelBinaryCache: fixtureBinaryCache{effects: effects},
		Clock:            func() time.Time { return time.Now().UTC() },
	})
}

type fixtureResolver struct{ effects bootFixtureEffects }

func fixtureAgentResolver(effects bootFixtureEffects) ports.AgentResolver {
	return fixtureResolver{effects: effects}
}
func (r fixtureResolver) Agent(harness domain.AgentHarness) (ports.Agent, bool) {
	if bootAgentDependencyNil(r.effects) {
		return nil, false
	}
	r.effects.Allow("agent.resolve")
	if harness != domain.HarnessCodex {
		return nil, false
	}
	return fixtureCatalogAgent{effects: r.effects}, true
}

type fixtureCatalogAgent struct{ effects bootFixtureEffects }

func (a fixtureCatalogAgent) GetConfigSpec(context.Context) (ports.ConfigSpec, error) {
	a.effects.Allow("agent.config")
	return ports.ConfigSpec{}, nil
}
func (a fixtureCatalogAgent) ResolveBinary(context.Context) (string, error) {
	a.effects.Allow("agent.readiness.binary")
	// A synthetic identifier, never an executable or PATH lookup.
	return "fixture://no-executable", nil
}
func (a fixtureCatalogAgent) ResolveBinaryPresence(ctx context.Context) (string, error) {
	return a.ResolveBinary(ctx)
}
func (a fixtureCatalogAgent) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	a.effects.Allow("agent.readiness.authentication")
	return ports.AgentAuthStatusAuthorized, nil
}
func (a fixtureCatalogAgent) GetLaunchCommand(context.Context, ports.LaunchConfig) ([]string, error) {
	return nil, fixtureDenied(a.effects, "agent.native.launch")
}
func (a fixtureCatalogAgent) GetRestoreCommand(context.Context, ports.RestoreConfig) ([]string, bool, error) {
	return nil, false, fixtureDenied(a.effects, "agent.native.restore")
}
func (a fixtureCatalogAgent) GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	a.effects.Allow("agent.prompt-strategy")
	return ports.PromptDeliveryInCommand, nil
}
func (a fixtureCatalogAgent) GetAgentHooks(context.Context, ports.WorkspaceHookConfig) error {
	return fixtureDenied(a.effects, "agent.native.hooks")
}
func (a fixtureCatalogAgent) SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error) {
	a.effects.Allow("agent.session-info")
	return ports.SessionInfo{}, false, nil
}

type fixtureModels struct{ effects bootFixtureEffects }

func fixtureModelCatalog() ports.AgentModelCatalog {
	return ports.AgentModelCatalog{
		AgentID: string(domain.HarnessCodex), Source: "offline-fixture",
		SelectionMode: ports.ModelSelectionCatalog, CustomModelEntry: ports.CustomModelEntryNone,
		Models: []ports.AgentModelInfo{{
			ID: fixtureAgentModel, Label: "Synthetic fixture model", IsDefault: true,
			Efforts: []string{fixtureAgentEffort}, DefaultEffort: fixtureAgentEffort,
		}},
		Metadata:  map[string]string{"synthetic": "true"},
		FetchedAt: time.Now().UTC(),
	}
}
func (m fixtureModels) Discover(_ context.Context, request ports.AgentModelDiscoveryRequest) (ports.AgentModelCatalog, error) {
	if request.AgentID != string(domain.HarnessCodex) {
		return ports.AgentModelCatalog{}, fixtureDenied(m.effects, "model.unsupported-agent")
	}
	m.effects.Allow("model.discover")
	return fixtureModelCatalog(), nil
}
func (m fixtureModels) CatalogFingerprint(context.Context, ports.AgentModelDiscoveryRequest) string {
	m.effects.Allow("model.fingerprint")
	return "offline-fixture-model-medium-v1"
}
func (m fixtureModels) Manual(string) ports.AgentModelCatalog {
	m.effects.Allow("model.manual-policy")
	return fixtureModelCatalog()
}

type fixtureBinaryCache struct{ effects bootFixtureEffects }

func (c fixtureBinaryCache) InvalidateBinaryResolution(domain.AgentHarness) {
	c.effects.Allow("model.binary.invalidate")
}

func fixtureWithin(root, target string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type fixtureAccountFactory struct {
	effects   bootFixtureEffects
	stateRoot string
}

func (f fixtureAccountFactory) Open(_ context.Context, account ports.CodexAccountContext) (ports.CodexAccountClient, error) {
	if !fixtureWithin(f.stateRoot, account.Home) {
		return nil, fixtureDenied(f.effects, "account.client.outside-fixture")
	}
	f.effects.Allow("account.client.open")
	return fixtureAccountClient{effects: f.effects}, nil
}
func (f fixtureAccountFactory) Capabilities(context.Context) domain.CodexAccountCapabilities {
	f.effects.Allow("account.capabilities")
	yes := domain.CodexCapabilityObservation{State: domain.CodexCapabilitySupported, ReasonCode: "offline_fixture", Reason: "Synthetic fixture observation"}
	no := domain.CodexCapabilityObservation{State: domain.CodexCapabilityUnsupported, ReasonCode: "offline_fixture_denied", Reason: "Fixture forbids credential mutation"}
	return domain.CodexAccountCapabilities{AccountRead: yes, CapacityRead: yes, UsageRead: yes, NativeLogin: no, ResetCreditConsume: no, GlobalSwitch: no}
}

type fixtureAccountClient struct{ effects bootFixtureEffects }

func (c fixtureAccountClient) Read(_ context.Context, refresh bool) (ports.CodexAccountObservation, error) {
	if refresh {
		return ports.CodexAccountObservation{}, fixtureDenied(c.effects, "account.credentials.refresh")
	}
	c.effects.Allow("account.identity.read")
	email := "synthetic-fixture@example.invalid"
	return ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodOther, Email: &email}, nil
}
func (c fixtureAccountClient) ReadCapacity(context.Context) (ports.CodexCapacityObservation, error) {
	c.effects.Allow("account.capacity.read")
	duration := int64(300)
	return ports.CodexCapacityObservation{
		Overall: &domain.CodexCapacityBucket{LimitID: "offline-fixture", Reached: domain.CodexCapacityNotReached,
			Primary: &domain.CodexCapacityWindow{UsedPercent: 20, WindowDurationMinutes: &duration}},
		ObservedAt: time.Now().UTC(),
	}, nil
}
func (c fixtureAccountClient) ReadUsage(context.Context) (ports.CodexUsageObservation, error) {
	c.effects.Allow("account.usage.read")
	zero := int64(0)
	return ports.CodexUsageObservation{LifetimeTokens: &zero, ObservedAt: time.Now().UTC()}, nil
}
func (c fixtureAccountClient) Logout(context.Context) error {
	return fixtureDenied(c.effects, "account.credentials.logout")
}
func (c fixtureAccountClient) ConsumeResetCredit(context.Context, string) (domain.CodexResetCreditOutcome, error) {
	return "", fixtureDenied(c.effects, "account.credit.reset")
}
func (c fixtureAccountClient) Events() <-chan ports.CodexAccountEvent {
	c.effects.Allow("account.events")
	ch := make(chan ports.CodexAccountEvent)
	close(ch)
	return ch
}
func (c fixtureAccountClient) Close() error { c.effects.Allow("account.client.close"); return nil }

type fixtureAccountProvider struct {
	effects   bootFixtureEffects
	stateRoot string
}

func (p fixtureAccountProvider) Provider() domain.AgentAccountProvider {
	return domain.AgentAccountProviderCodex
}
func (p fixtureAccountProvider) validHome(home string) bool {
	return home == "" || fixtureWithin(p.stateRoot, home)
}
func (p fixtureAccountProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if !p.validHome(home) {
		return domain.AgentAccountIdentityCheck{}, fixtureDenied(p.effects, "account.provider.outside-fixture")
	}
	p.effects.Allow("account.provider.identity")
	return domain.AgentAccountIdentityCheck{LoggedIn: true, SignedIn: true, Email: "synthetic-fixture@example.invalid"}, nil
}
func (p fixtureAccountProvider) Limits(_ context.Context, home string) (domain.AgentAccountLimits, error) {
	if !p.validHome(home) {
		return domain.AgentAccountLimits{}, fixtureDenied(p.effects, "account.provider.outside-fixture")
	}
	p.effects.Allow("account.provider.limits")
	return domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{ID: "offline-fixture", UsedPercent: 20, DurationMinutes: 300}}, FetchedAt: time.Now().UTC()}, nil
}
func (p fixtureAccountProvider) LoginArgv(context.Context) ([]string, error) {
	return nil, fixtureDenied(p.effects, "account.provider.login")
}
func (p fixtureAccountProvider) Logout(context.Context, string) error {
	return fixtureDenied(p.effects, "account.provider.logout")
}
func (p fixtureAccountProvider) Prepare(string) error {
	return fixtureDenied(p.effects, "account.provider.prepare")
}
func (p fixtureAccountProvider) LaunchEnv(home string) map[string]string {
	if home != "" {
		_ = fixtureDenied(p.effects, "account.provider.managed-launch")
		return nil
	}
	p.effects.Allow("account.provider.device-env")
	return map[string]string{"AO_SYNTHETIC_FIXTURE": "1"}
}

type fixtureAccountTerminals struct{ effects bootFixtureEffects }

func (t fixtureAccountTerminals) OpenCommandTerminal(context.Context, shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	return shellterm.ShellTerminal{}, fixtureDenied(t.effects, "account.terminal.open")
}
func (t fixtureAccountTerminals) CloseShellTerminal(context.Context, string) error {
	return fixtureDenied(t.effects, "account.terminal.close")
}

// Synthetic ownership records deliberately contain no physical process IDs.
// Only the parent fixture seeds them. Runtime.Create is always denied, so a
// recovery path that abandons an ambiguous owner cannot manufacture success.
type fixtureRuntimeOwner struct {
	Version    int
	Synthetic  bool
	Handle     string
	SessionID  domain.SessionID
	Generation string
	State      ports.FencedLiveness
}
type fixtureTerminalRuntime struct {
	effects bootFixtureEffects
	root    string
	mu      sync.Mutex
}

var _ runtimeselect.Runtime = (*fixtureTerminalRuntime)(nil)

func fixtureRuntime(effects bootFixtureEffects, root string) runtimeselect.Runtime {
	return &fixtureTerminalRuntime{effects: effects, root: root}
}
func fixtureRuntimeHandle(id domain.SessionID) ports.RuntimeHandle {
	sum := sha256.Sum256([]byte(id))
	return ports.RuntimeHandle{ID: "fixture-runtime-" + hex.EncodeToString(sum[:])}
}
func fixtureRuntimeDirectory(root string, create bool) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("fixture runtime root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("fixture runtime root is missing or aliased")
	}
	physical, err := filepath.EvalSymlinks(root)
	if err != nil || !strings.EqualFold(filepath.Clean(physical), filepath.Clean(root)) {
		return "", errors.New("fixture runtime root has an aliased ancestor")
	}
	dir := filepath.Join(root, "runtime-owners")
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	info, err = os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && !create {
		return dir, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("fixture ownership directory is unavailable or aliased")
	}
	return dir, nil
}
func fixtureRuntimeRecordPath(root string, handle ports.RuntimeHandle) (string, error) {
	key, ok := strings.CutPrefix(handle.ID, "fixture-runtime-")
	if !ok || len(key) != 64 {
		return "", ports.ErrRuntimeProbeInconclusive
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", ports.ErrRuntimeProbeInconclusive
	}
	dir, err := fixtureRuntimeDirectory(root, false)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}
func seedFixtureRuntimeOwner(root string, id domain.SessionID, generation string, state ports.FencedLiveness) (ports.RuntimeHandle, error) {
	handle := fixtureRuntimeHandle(id)
	if id == "" || generation == "" || (state != ports.FencedAlive && state != ports.FencedDead && state != ports.FencedUnknown) {
		return ports.RuntimeHandle{}, errors.New("fixture owner requires explicit session, generation and state")
	}
	if _, err := fixtureRuntimeDirectory(root, true); err != nil {
		return ports.RuntimeHandle{}, err
	}
	path, err := fixtureRuntimeRecordPath(root, handle)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	data, err := json.Marshal(fixtureRuntimeOwner{Version: 1, Synthetic: true, Handle: handle.ID, SessionID: id, Generation: generation, State: state})
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return ports.RuntimeHandle{}, err
	}
	return handle, nil
}
func (r *fixtureTerminalRuntime) load(handle ports.RuntimeHandle) (fixtureRuntimeOwner, bool, error) {
	path, err := fixtureRuntimeRecordPath(r.root, handle)
	if err != nil {
		return fixtureRuntimeOwner{}, false, fmt.Errorf("%w: invalid fixture ownership path", ports.ErrRuntimeProbeInconclusive)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fixtureRuntimeOwner{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 65536 {
		return fixtureRuntimeOwner{}, false, ports.ErrRuntimeProbeInconclusive
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fixtureRuntimeOwner{}, false, fmt.Errorf("%w: fixture owner unreadable", ports.ErrRuntimeProbeInconclusive)
	}
	var owner fixtureRuntimeOwner
	if err := json.Unmarshal(data, &owner); err != nil || owner.Version != 1 || !owner.Synthetic || owner.Handle != handle.ID || fixtureRuntimeHandle(owner.SessionID) != handle || owner.Generation == "" ||
		(owner.State != ports.FencedAlive && owner.State != ports.FencedDead && owner.State != ports.FencedUnknown) {
		return fixtureRuntimeOwner{}, false, ports.ErrRuntimeProbeInconclusive
	}
	return owner, true, nil
}
func (r *fixtureTerminalRuntime) Create(context.Context, ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	return ports.RuntimeHandle{}, fixtureDenied(r.effects, "runtime.create")
}
func (r *fixtureTerminalRuntime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, found, err := r.load(handle)
	if err != nil {
		return err
	}
	if !found {
		r.effects.Allow("runtime.destroy.absent")
		return nil
	}
	if owner.State == ports.FencedUnknown {
		return ports.ErrRuntimeProbeInconclusive
	}
	owner.State = ports.FencedDead
	path, err := fixtureRuntimeRecordPath(r.root, handle)
	if err != nil {
		return err
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "fixture-owner-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	r.effects.Allow("runtime.destroy.synthetic")
	return nil
}
func (r *fixtureTerminalRuntime) IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.effects.Allow("runtime.probe")
	owner, found, err := r.load(handle)
	if err != nil || !found {
		return false, err
	}
	if owner.State == ports.FencedUnknown {
		return false, ports.ErrRuntimeProbeInconclusive
	}
	return owner.State == ports.FencedAlive, nil
}
func (r *fixtureTerminalRuntime) IsChildAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	return r.IsAlive(ctx, handle)
}
func (r *fixtureTerminalRuntime) ProbeFencedRuntime(ctx context.Context, ref ports.FencedRuntimeRef) ports.FencedProbeResult {
	r.effects.Allow("runtime.probe.fenced")
	if ctx.Err() != nil {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
	}
	if ref.SessionID == "" || ref.Generation == "" || ref.NativeIdentity != "" || ref.Handle != fixtureRuntimeHandle(ref.SessionID) {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonIdentityMissing}
	}
	owner, found, err := r.load(ref.Handle)
	if err != nil {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonRegistryUnreadable}
	}
	if !found {
		return ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}
	}
	if owner.SessionID != ref.SessionID || owner.Generation != ref.Generation {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonGenerationMismatch}
	}
	if owner.State == ports.FencedUnknown {
		return ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
	}
	reason := ports.FencedReasonExactMatch
	if owner.State == ports.FencedDead {
		reason = ports.FencedReasonExactAbsent
	}
	return ports.FencedProbeResult{Liveness: owner.State, Reason: reason}
}
func (r *fixtureTerminalRuntime) IsExactSupervisedProcessAlive(ctx context.Context, handle ports.RuntimeHandle, ref ports.SupervisedProcessRef) (bool, error) {
	probe := r.ProbeFencedRuntime(ctx, ports.FencedRuntimeRef{Handle: handle, SessionID: ref.SessionID, Generation: ref.LaunchID})
	if probe.Liveness == ports.FencedUnknown {
		return false, ports.ErrRuntimeProbeInconclusive
	}
	return probe.Liveness == ports.FencedAlive, nil
}
func (r *fixtureTerminalRuntime) HasSupervisedProcessRecord(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.effects.Allow("runtime.supervisor.record")
	_, found, err := r.load(handle)
	return found, err
}
func (r *fixtureTerminalRuntime) ProcessRootPIDs(context.Context, ports.RuntimeHandle) ([]int, error) {
	r.effects.Allow("runtime.physical-pids.unknown")
	return nil, ports.ErrRuntimeProbeInconclusive
}
func (r *fixtureTerminalRuntime) ServerPID(context.Context) (int, bool) {
	r.effects.Allow("runtime.server-pid.absent")
	return 0, false
}
func (r *fixtureTerminalRuntime) Attach(context.Context, ports.RuntimeHandle, uint16, uint16) (ports.Stream, error) {
	return nil, fixtureDenied(r.effects, "runtime.attach")
}
func (r *fixtureTerminalRuntime) Interrupt(context.Context, ports.RuntimeHandle) error {
	return fixtureDenied(r.effects, "runtime.interrupt")
}
func (r *fixtureTerminalRuntime) SendInput(context.Context, ports.RuntimeHandle, string) error {
	return fixtureDenied(r.effects, "runtime.input")
}
func (r *fixtureTerminalRuntime) SendMessage(context.Context, ports.RuntimeHandle, string) error {
	return fixtureDenied(r.effects, "runtime.message")
}
func (r *fixtureTerminalRuntime) GetOutput(ctx context.Context, handle ports.RuntimeHandle, _ int) (string, error) {
	alive, err := r.IsAlive(ctx, handle)
	if err != nil {
		return "", err
	}
	if !alive {
		return "", ports.ErrRuntimeProcessExited
	}
	r.effects.Allow("runtime.output.synthetic")
	return "synthetic fixture ownership; no OS process", nil
}
