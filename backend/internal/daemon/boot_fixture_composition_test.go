package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	chatdriverregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/runtimeselect"
	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/filesnapshot"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/previewserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/push"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/computeruse"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubpat"
	importsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/importer"
	linkpreviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/linkpreview"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionsearch"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type fixtureEffectEntry struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}
type fixtureEffectRecorder struct {
	mu       sync.Mutex
	file     *os.File
	writeErr error
	entries  []fixtureEffectEntry
}

func newFixtureEffectRecorder(path string) (*fixtureEffectRecorder, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &fixtureEffectRecorder{file: file}, nil
}
func (e *fixtureEffectRecorder) record(kind, name, address string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := fixtureEffectEntry{Kind: kind, Name: name, Address: address}
	e.entries = append(e.entries, entry)
	if e.file != nil && e.writeErr == nil {
		body, err := json.Marshal(entry)
		if err == nil {
			_, err = e.file.Write(append(body, '\n'))
		}
		if err == nil {
			err = e.file.Sync()
		}
		e.writeErr = err
	}
}
func (e *fixtureEffectRecorder) Allow(name string) { e.record("allowed", name, "") }
func (e *fixtureEffectRecorder) Deny(name string) error {
	e.record("forbidden", name, "")
	return fmt.Errorf("fixture denied effect: %s", name)
}
func (e *fixtureEffectRecorder) Observe(event bootEvent) {
	e.record("lifecycle", event.Name, event.Address)
}
func (e *fixtureEffectRecorder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var err error
	if e.file != nil {
		err = e.file.Close()
		e.file = nil
	}
	return errors.Join(e.writeErr, err)
}

type fixtureDeniedTransport struct{ effects bootFixtureEffects }

func (t fixtureDeniedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.effects.Deny("network.http-unconfigured")
}

type fixtureTelemetry struct{ effects bootFixtureEffects }

func (s fixtureTelemetry) Emit(context.Context, ports.TelemetryEvent) {
	s.effects.Allow("telemetry.local-no-export")
}
func (s fixtureTelemetry) Close(context.Context) error {
	s.effects.Allow("telemetry.close")
	return nil
}

func fixtureConfig(root, runID string) config.Config {
	return config.Config{
		Host: "127.0.0.1", Port: 0, DataDir: filepath.Join(root, "state"), StateDir: filepath.Join(root, "state"), RunFilePath: filepath.Join(root, "state", "running.json"),
		Agent: "codex", AppRunID: runID, RequestTimeout: 10 * time.Second, ShutdownTimeout: 10 * time.Second, InstallerShutdownTimeout: 10 * time.Second,
		SessionAdmission: domain.SessionAdmissionLimits{RegisteredSlots: 40, MaxActiveWorkers: 40, DefaultProjectWorkers: 40, QueueEnabled: true},
		AllowedOrigins:   []string{}, LocalOffering: true, CloudOffering: false, HarnessUpdateChecks: false, UpdateWhileRunning: false, UpdateWhenIdle: false, VersionRefresh: false, TUIVersionRefresh: false,
	}
}

// This complete composition is intentionally independent of
// productionBootDependencies. No field is populated from production defaults.
func fixtureBootDependencies(root, runID, phase string, effects *fixtureEffectRecorder) bootDependencies {
	cfg := fixtureConfig(root, runID)
	return bootDependencies{
		HTTPMiddleware: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !fixtureHTTPAllowed(r.Method, r.URL.Path) {
					effects.Deny("http.unconfigured-route")
					http.Error(w, "fixture route denied", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
		Observe:             effects.Observe,
		FileSnapshotCapture: func() filesnapshot.CaptureFunc { return fixtureFileSnapshotCapture(effects) },
		LegacyImport: func(store *sqlite.Store) importsvc.Service {
			return importsvc.New(importsvc.Deps{Store: store, Root: filepath.Join(root, "legacy-import")})
		},
		LoadConfig: func() (config.Config, error) {
			if os.Getenv("AO_LOCAL_LIFECYCLE_TEST") != "" {
				return config.Config{}, errors.New("partial lifecycle bypass is forbidden in full fixture")
			}
			effects.Allow("config.explicit")
			return cfg, nil
		},
		HostCommands: func(string) bootHostCommands { return fixtureHostCommands(effects) },
		Installer: func(c config.Config, s *sqlite.Store, a ports.AgentResolver, h bootHostCommands) (*systeminstall.Service, error) {
			return fixtureInstaller(c, s, a, h, effects)
		},
		Preview: func(log *slog.Logger, dir string) (*previewserver.Manager, error) {
			return fixturePreview(log, dir, effects)
		},
		UsagePricing: func(c usagePricingRuntimeConfig) (*usagePricingRuntime, error) {
			return fixtureUsagePricing(c, effects)
		},
		UsageRoots: func(ctx context.Context, dir string) (usagesvc.SourceRoots, error) {
			return fixtureUsageRoots(ctx, dir, effects)
		},
		SessionSearch: func(ctx context.Context, s *sqlite.Store, dir string, log *slog.Logger) (*sessionsearch.Service, error) {
			return fixtureSessionSearch(ctx, s, dir, effects, log)
		},
		Reliability: func(ctx context.Context, c config.Config, s *sqlite.Store, l sessionLifecycle, a ports.CodexAccountClientFactory, home string, log *slog.Logger) (*httpd.ReliabilityDeps, error) {
			return fixtureReliability(ctx, c, s, l, a, home, log, effects)
		},
		SessionImport: func(s *sqlite.Store, dir string, supports func(domain.AgentHarness) bool) (*sessionimport.Service, error) {
			return sessionimport.NewWithSourceRoots(s, dir, filepath.Join(root, "import-home"), map[domain.AgentHarness]string{}, supports)
		},
		LinkPreview: func() *linkpreviewsvc.Service {
			return linkpreviewsvc.New(&http.Client{Transport: fixtureDeniedTransport{effects}})
		},
		GitHubStatus:           func(string, *scmmulti.Provider) controllers.GitHubPATService { return fixturePAT{effects} },
		BranchStateGit:         func() sessionsvc.BranchStateGit { return fixtureBranchStateGit(effects) },
		SCMRepositoryDiscovery: func() scmobserve.RepositoryDiscovery { return fixtureRepositoryDiscovery(effects) },
		ReapForkSeeds:          fixtureReapForkSeeds(effects),
		Chrome:                 func(config.Config, *slog.Logger) *chromeMode { effects.Allow("browser.native-disabled"); return nil },
		Telemetry: func(config.Config, *sqlite.Store, *slog.Logger) (ports.EventSink, ports.TelemetryIdentityStore, bool) {
			return fixtureTelemetry{effects}, nil, false
		},
		InitSentry:  func(sentryobs.Config) error { effects.Allow("sentry.disabled"); return nil },
		FlushSentry: func(time.Duration) { effects.Allow("sentry.flush-disabled") },
		IdentityLookup: func(context.Context, config.Config, ports.TelemetryIdentityStore, *slog.Logger) {
			effects.Allow("identity.lookup-disabled")
		},
		StopProviderHost: func(context.Context, string, string) error { return effects.Deny("providerhost.stop") },
		ProviderHostPID:  func(string, string) (int, bool) { effects.Allow("providerhost.synthetic-no-pid"); return 0, false },
		ReconcileProviderHosts: func(context.Context, string, *sqlite.Store, string) error {
			effects.Allow("providerhost.synthetic-empty-reconcile")
			return nil
		},
		ComputerDriver:  func() computeruse.Driver { return fixtureComputer{effects} },
		ContainerReaper: func() ports.ContainerReaper { return fixtureContainers{effects} },
		Mobile: func(config.Config, *controllers.BridgeService, *systeminstall.Service, string, *slog.Logger) mobileBootEffects {
			return fixtureMobileEffects(effects)
		},
		MobileLAN: func(http.Handler, string, int, *slog.Logger, ports.EventSink) bootMobileLAN {
			return &fixtureLAN{effects: effects}
		},
		AwakeHost: func(int, string, string) bootAwakeHost { return fixtureAwake{effects} },
		ShutdownContext: func() (context.Context, context.CancelFunc, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel, func() {}
		},
		Runtime:               func(*slog.Logger, string) runtimeselect.Runtime { return fixtureRuntime(effects, root) },
		AgentResolver:         func(string, *slog.Logger) (ports.AgentResolver, error) { return fixtureAgentResolver(effects), nil },
		AccountLimitsInterval: func() time.Duration { return 20 * time.Millisecond },
		AgentServices: func(ctx context.Context, c config.Config, s *sqlite.Store, g ports.CodexOperationGate, log *slog.Logger) (agentBootComponents, error) {
			return fixtureAgentComponents(ctx, c, s, g, log, effects)
		},
		ChatDrivers: func(*slog.Logger, func()) *chatdriverregistry.Registry {
			return chatdriverregistry.New(&fixtureChatDriver{effects: effects, root: root, phase: phase})
		},
		Workspace: func(config.Config, *sqlite.Store, *slog.Logger) (ports.Workspace, error) {
			return fixtureWorkspace(filepath.Join(root, "workspaces"), effects), nil
		},
		Reviewers:   func() (bootReviewerResolver, error) { return fixtureReviewers(effects), nil },
		Tracker:     func(config.GitLabConfig, *slog.Logger) ports.Tracker { return fixtureTracker(effects) },
		SCM:         func(config.GitLabConfig, *slog.Logger) *scmmulti.Provider { return fixtureSCM(effects) },
		Merger:      func(config.GitLabConfig, *slog.Logger) *scmmulti.Merger { return fixtureMerger(effects) },
		GitHubRepos: func(*slog.Logger) ports.GitHubRepositoryResolver { return fixtureGitHubRepos(effects) },
	}
}

type fixtureComputer struct{ e bootFixtureEffects }

func (f fixtureComputer) Locate(context.Context) (string, error) {
	f.e.Allow("computer.not-installed")
	return "", ports.ErrComputerUseDriverNotFound
}
func (f fixtureComputer) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	return nil, nil, f.e.Deny("computer.run")
}
func (f fixtureComputer) StartDaemon(context.Context, string) error {
	return f.e.Deny("computer.start")
}

type fixtureContainers struct{ e bootFixtureEffects }

func (f fixtureContainers) ReapSessionContainers(context.Context, domain.SessionID) (int, error) {
	f.e.Allow("containers.synthetic-empty")
	return 0, nil
}

type fixturePAT struct{ e bootFixtureEffects }

func (f fixturePAT) StorePAT(context.Context, string) error { return f.e.Deny("github.store-token") }
func (f fixturePAT) StoreOAuthToken(context.Context, string, string, time.Time, time.Time) error {
	return f.e.Deny("github.store-oauth")
}
func (f fixturePAT) HasPAT(context.Context) bool {
	f.e.Allow("github.synthetic-no-token")
	return false
}
func (f fixturePAT) Login(context.Context) string    { f.e.Allow("github.synthetic-no-login"); return "" }
func (f fixturePAT) DeletePAT(context.Context) error { return f.e.Deny("github.delete-token") }
func (f fixturePAT) ListRepos(context.Context) ([]githubpat.Repo, error) {
	return nil, f.e.Deny("github.list-repos")
}

type fixturePush struct{ e bootFixtureEffects }

func (f fixturePush) Send(context.Context, []push.Message) ([]push.Ticket, error) {
	return nil, f.e.Deny("push.send")
}
func (f fixturePush) GetReceipts(context.Context, []string) (map[string]push.Receipt, error) {
	return nil, f.e.Deny("push.receipts")
}
func fixtureMobileEffects(e bootFixtureEffects) mobileBootEffects {
	return mobileBootEffects{
		PickLANHosts:       func() []string { e.Allow("mobile.lan-disabled"); return nil },
		PickTailscaleHosts: func() []string { e.Allow("mobile.tailscale-disabled"); return nil },
		ApplyServe:         func(int) error { return e.Deny("mobile.serve-start") },
		ClearServe:         func() error { return e.Deny("mobile.serve-clear") },
		QueryTS: func() mobilebridge.TailscaleInfo {
			e.Allow("mobile.synthetic-no-tailscale")
			return mobilebridge.TailscaleInfo{}
		},
		ServeTarget:       func() int { e.Allow("mobile.synthetic-no-target"); return 0 },
		ReapTunnel:        func() error { e.Allow("mobile.synthetic-no-tunnel"); return nil },
		ResolveTunnel:     func(bool) controllers.TunnelController { e.Allow("mobile.tunnel-unsupported"); return nil },
		KeepAwake:         func() controllers.KeepAwakeController { return nil },
		PushSender:        func() push.Sender { return fixturePush{e} },
		PublishRemoteHost: func(context.Context, *controllers.BridgeService) { e.Allow("mobile.publication-disabled") },
	}
}

type fixtureLAN struct {
	effects  bootFixtureEffects
	password string
}

func (f *fixtureLAN) Start(int) (int, error)         { return 0, f.effects.Deny("lan.start") }
func (f *fixtureLAN) StartLoopback(int) (int, error) { return 0, f.effects.Deny("lan.start-loopback") }
func (f *fixtureLAN) Stop(context.Context) error     { f.effects.Allow("lan.stop-inert"); return nil }
func (f *fixtureLAN) Running() bool                  { return false }
func (f *fixtureLAN) BoundPort() int                 { return 0 }
func (f *fixtureLAN) SetPasswordHash(s string)       { f.password = s }
func (f *fixtureLAN) PasswordHash() string           { return f.password }
func (f *fixtureLAN) UseDeviceCredentials(*mobilebridge.AccessDevices, *mobilebridge.PairingCodes) {
	f.effects.Allow("lan.synthetic-credentials")
}

type fixtureAwake struct{ e bootFixtureEffects }

func (f fixtureAwake) Supported() bool                     { return false }
func (f fixtureAwake) Active() bool                        { return false }
func (f fixtureAwake) Start() error                        { return f.e.Deny("awake.start") }
func (f fixtureAwake) Stop()                               { f.e.Allow("awake.stop-inert") }
func (f fixtureAwake) ConfigureLid(bool, bool) error       { f.e.Allow("awake.configure-inert"); return nil }
func (f fixtureAwake) SetupLid() error                     { return f.e.Deny("awake.lid-setup") }
func (f fixtureAwake) LidStatus() (bool, bool, string)     { return false, false, "synthetic unsupported" }
func (f fixtureAwake) ConfigureIdle(time.Time, bool) error { f.e.Allow("awake.idle-inert"); return nil }
func (f fixtureAwake) SetDeadline(time.Time) error         { f.e.Allow("awake.deadline-inert"); return nil }
func (f fixtureAwake) SetProtection(int) error             { f.e.Allow("awake.protection-inert"); return nil }
func (f fixtureAwake) ConfigureEffects(domain.AwakeSettings) error {
	f.e.Allow("awake.effects-inert")
	return nil
}
func (f fixtureAwake) RequestRecovery() error { return f.e.Deny("awake.recovery") }
func (f fixtureAwake) RecoveryReason() string { return "" }
func (f fixtureAwake) RelaunchBrowser(context.Context, string) error {
	return f.e.Deny("awake.browser-launch")
}
func (f fixtureAwake) JoinNetwork(context.Context, string) error {
	return f.e.Deny("awake.network-join")
}
func (f fixtureAwake) Observe(context.Context) (domain.AwakeObservation, error) {
	f.e.Allow("awake.synthetic-observe")
	return domain.AwakeObservation{}, nil
}
func (f fixtureAwake) Capabilities() []domain.AwakeCapability { return nil }
func (f fixtureAwake) AppAlive(context.Context, string, string) (bool, string, error) {
	return false, "", f.e.Deny("awake.native-app-observation")
}
func fixtureHTTPAllowed(method, path string) bool {
	switch method + " " + path {
	case "GET /healthz", "GET /api/v1/sessions", "GET /internal/reliability", "PUT /internal/reliability/inference", "POST /shutdown",
		"POST /api/v1/sessions/fixture-project-2/conversation/messages", "GET /api/v1/sessions/fixture-project-2/conversation",
		"POST /api/v1/sessions/fixture-project-3/conversation/messages", "GET /api/v1/sessions/fixture-project-3/conversation":
		return true
	default:
		return false
	}
}
