package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/fleetpressure"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/previewserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/pricing"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionsearch"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// hostFixtureEffects is implemented by the subprocess fixture's durable
// recorder. Every deliberately synthetic observation is distinguished from a
// forbidden operation; a swallowed denial still fails the parent assertion.
type hostFixtureEffects interface {
	Allow(string)
	Deny(string) error
}

type fixtureHostCommandAdapter struct{ effects hostFixtureEffects }

func fixtureHostCommands(effects hostFixtureEffects) bootHostCommands {
	return &fixtureHostCommandAdapter{effects: effects}
}
func (h *fixtureHostCommandAdapter) LookPath(string) (string, error) {
	return "", h.effects.Deny("host.look_path")
}
func (h *fixtureHostCommandAdapter) Run(context.Context, []string, io.Writer, io.Writer) error {
	return h.effects.Deny("host.command")
}
func (h *fixtureHostCommandAdapter) RunInstall(context.Context, ports.InstallCommand, io.Writer, io.Writer) error {
	return h.effects.Deny("host.install_command")
}
func (h *fixtureHostCommandAdapter) RunInstallScript(context.Context, ports.InstallScriptCommand, io.Writer, io.Writer) (ports.InstallScriptResult, error) {
	return ports.InstallScriptResult{}, h.effects.Deny("host.install_script")
}
func (h *fixtureHostCommandAdapter) InstallManagedBinary(context.Context, ports.ManagedBinaryDownload) (string, error) {
	return "", h.effects.Deny("host.install_binary")
}
func (h *fixtureHostCommandAdapter) Probe(context.Context) (ports.InstallCapabilities, error) {
	return ports.InstallCapabilities{}, h.effects.Deny("host.capability_probe")
}
func (h *fixtureHostCommandAdapter) PathWritable(context.Context, string) (bool, error) {
	return false, h.effects.Deny("host.path_writable")
}

func fixtureInstaller(cfg config.Config, store *sqlite.Store, agents ports.AgentResolver, commands bootHostCommands, effects hostFixtureEffects) (*systeminstall.Service, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("fixture installer requires an absolute data root")
	}
	return systeminstall.NewWithExternalEffects(commands, commands, systeminstall.Deps{
		JobStore: store, Verifier: systeminstall.NewVerifier(agents, commands),
		Sessions: store, Reviewers: store, Releases: store,
		DisableUpdateChecks:       !cfg.HarnessUpdateChecks,
		DisableUpdateWhileRunning: !cfg.UpdateWhileRunning,
		UpdateWhenIdle:            cfg.UpdateWhenIdle,
		ManagedRoot:               managedHarnessRoot(cfg),
		PrivateNPMPrefixes: map[systeminstall.Target]string{
			systeminstall.TargetOpencodeV2: filepath.Join(cfg.DataDir, "fixture-install", "npm"),
		},
		PrivateInstallerHomes: map[systeminstall.Target]string{
			systeminstall.TargetOpencodeV2: filepath.Join(cfg.DataDir, "fixture-install", "home"),
		},
	}, systeminstall.ExternalEffects{
		ManagedVersion: func(context.Context, systeminstall.Plan) (string, error) {
			return "", effects.Deny("installer.managed_version")
		},
		OfficialVersion: func(context.Context, systeminstall.Target) (string, error) {
			return "", effects.Deny("installer.official_version")
		},
		OwnsInstallation: func(context.Context, string, string, string, bool) (bool, error) {
			return false, effects.Deny("installer.owns_installation")
		},
	})
}

func fixturePreview(log *slog.Logger, dataDir string, effects hostFixtureEffects) (*previewserver.Manager, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("fixture preview requires an absolute data root")
	}
	return previewserver.NewWithProcessEffects(log, dataDir, previewserver.ProcessEffects{
		Command:      func(string, ...string) (*exec.Cmd, error) { return nil, effects.Deny("preview.command") },
		StartTime:    func(int) string { _ = effects.Deny("preview.process_identity"); return "" },
		Terminate:    func(*exec.Cmd, string) error { return effects.Deny("preview.terminate") },
		ForceKill:    func(*exec.Cmd, string) error { return effects.Deny("preview.force_kill") },
		ForceKillPID: func(int, string) error { return effects.Deny("preview.orphan_kill") },
	})
}

type fixtureCatalogFetcher struct{ effects hostFixtureEffects }

func (f fixtureCatalogFetcher) Fetch(ctx context.Context, _ string, _ bool) (pricing.FetchResult, error) {
	if err := ctx.Err(); err != nil {
		return pricing.FetchResult{}, err
	}
	f.effects.Allow("pricing.catalog_fetch")
	// Unavailable is an honest synthetic observation. No catalog or cost is
	// invented and the real refresher's bounded retry policy remains exercised.
	return pricing.FetchResult{}, errors.New("synthetic catalog unavailable")
}
func fixtureUsagePricing(cfg usagePricingRuntimeConfig, effects hostFixtureEffects) (*usagePricingRuntime, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("fixture pricing requires an absolute data root")
	}
	cfg.Fetcher = fixtureCatalogFetcher{effects}
	return newUsagePricingRuntimeWithFetcher(cfg)
}
func fixtureUsageRoots(ctx context.Context, dataDir string, effects hostFixtureEffects) (usagesvc.SourceRoots, error) {
	if err := ctx.Err(); err != nil {
		return usagesvc.SourceRoots{}, err
	}
	if !filepath.IsAbs(dataDir) {
		return usagesvc.SourceRoots{}, errors.New("fixture usage requires an absolute data root")
	}
	effects.Allow("usage.source_roots")
	root := filepath.Join(dataDir, "fixture-native-usage")
	return usagesvc.SourceRoots{
		ClaudeProjects:      filepath.Join(root, "claude-projects"),
		CodexSessions:       filepath.Join(root, "codex-sessions"),
		CodexArchived:       filepath.Join(root, "codex-archived"),
		KimiHome:            filepath.Join(root, "kimi"),
		PiSessions:          filepath.Join(root, "pi-sessions"),
		PiAgentSessions:     filepath.Join(root, "pi-agent-sessions"),
		QwenUsage:           filepath.Join(root, "qwen"),
		OMPSessions:         filepath.Join(root, "omp"),
		KimchiSessions:      filepath.Join(root, "kimchi"),
		PrimeAgentSessions:  filepath.Join(root, "prime"),
		GooseSessions:       filepath.Join(root, "goose"),
		OpenCodeData:        filepath.Join(root, "opencode"),
		KiloData:            filepath.Join(root, "kilo"),
		GrokSessions:        filepath.Join(root, "grok"),
		DeepSeekSessions:    filepath.Join(root, "deepseek"),
		AgyCLIConversations: filepath.Join(root, "agy-cli"),
		AgyACPConversations: filepath.Join(root, "agy-acp"),
	}, nil
}
func fixtureSessionSearch(ctx context.Context, store *sqlite.Store, stateDir string, effects hostFixtureEffects, log *slog.Logger) (*sessionsearch.Service, error) {
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("fixture search requires an absolute state root")
	}
	effects.Allow("search.source_roots")
	root := filepath.Join(stateDir, "fixture-search-transcripts")
	return startSessionSearchWithRoots(ctx, store, sessionsearch.TranscriptRoots{
		ClaudeProjects: filepath.Join(root, "claude-projects"),
		CodexSessions:  filepath.Join(root, "codex-sessions"),
		CodexArchived:  filepath.Join(root, "codex-archived"),
		OpenCodeData:   filepath.Join(root, "opencode"),
		OpenCodeV2Data: filepath.Join(root, "opencode-v2"),
		AOStateDir:     stateDir,
	}, log)
}

type fixtureHostSampler struct {
	mu      sync.Mutex
	at      time.Time
	effects hostFixtureEffects
}

func (s *fixtureHostSampler) Sample(ctx context.Context, at time.Time) (fleetpressure.Measurement, error) {
	if err := ctx.Err(); err != nil {
		return fleetpressure.Measurement{At: at}, err
	}
	s.effects.Allow("reliability.host_sample")
	s.mu.Lock()
	s.at = at
	s.mu.Unlock()
	available := uint64(64 << 30)
	cpu := float64(1)
	return fleetpressure.Measurement{At: at, AvailableBytes: &available, CPUPercent: &cpu}, nil
}
func (s *fixtureHostSampler) HandleObservation() fleetpressure.HandleObservation {
	s.mu.Lock()
	at := s.at
	s.mu.Unlock()
	count := 100
	return fleetpressure.HandleObservation{At: at, Count: &count, Scope: "synthetic_fixture", FleetComplete: true, CoverageReason: "explicit synthetic runtime inventory"}
}
func fixtureReliability(ctx context.Context, cfg config.Config, store *sqlite.Store, lifecycle sessionLifecycle, accounts ports.CodexAccountClientFactory, accountHome string, log *slog.Logger, effects hostFixtureEffects) (*httpd.ReliabilityDeps, error) {
	return startReliabilityWithObservations(ctx, cfg, store, lifecycle, accounts, accountHome, log, reliabilityObservations{
		Host: &fixtureHostSampler{effects: effects},
		WorktreeCount: func(ctx context.Context, _ *sqlite.Store) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			effects.Allow("reliability.worktree_count")
			// The injected workspace adapter creates directories, not Git worktrees.
			return 0, nil
		},
	})
}
