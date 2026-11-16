package daemon

import (
	"context"
	"fmt"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/systemexec"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/filesnapshot"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/previewserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubpat"
	importsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/importer"
	linkpreviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/linkpreview"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionsearch"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/container/dockerreap"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/cuadriver"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
	awakesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/awake"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/computeruse"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"

	chatdriverregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/reviewer"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/runtimeselect"
	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// bootDependencies is the private composition boundary used by Run and its
// subprocess recovery tests. Each field is required. There is deliberately no
// "fill defaults" operation: an explicit incomplete composition fails before
// configuration loading, database access, goroutines, or any native effect.
//
// Production Run always selects productionBootDependencies. Tests build an
// independent explicit set in _test.go; no production flag selects fake agents.
type bootEvent struct {
	Name    string
	Address string
}

type bootDependencies struct {
	HTTPMiddleware         func(http.Handler) http.Handler
	FileSnapshotCapture    func() filesnapshot.CaptureFunc
	LegacyImport           func(*sqlite.Store) importsvc.Service
	Observe                func(bootEvent)
	LoadConfig             func() (config.Config, error)
	HostCommands           func(string) bootHostCommands
	Installer              func(config.Config, *sqlite.Store, ports.AgentResolver, bootHostCommands) (*systeminstall.Service, error)
	Preview                func(*slog.Logger, string) (*previewserver.Manager, error)
	UsagePricing           func(usagePricingRuntimeConfig) (*usagePricingRuntime, error)
	UsageRoots             func(context.Context, string) (usagesvc.SourceRoots, error)
	SessionSearch          func(context.Context, *sqlite.Store, string, *slog.Logger) (*sessionsearch.Service, error)
	Reliability            func(context.Context, config.Config, *sqlite.Store, sessionLifecycle, ports.CodexAccountClientFactory, string, *slog.Logger) (*httpd.ReliabilityDeps, error)
	SessionImport          func(*sqlite.Store, string, func(domain.AgentHarness) bool) (*sessionimport.Service, error)
	LinkPreview            func() *linkpreviewsvc.Service
	GitHubStatus           func(string, *scmmulti.Provider) controllers.GitHubPATService
	BranchStateGit         func() sessionsvc.BranchStateGit
	SCMRepositoryDiscovery func() scmobserve.RepositoryDiscovery
	ReapForkSeeds          func(context.Context, *sqlite.Store, *slog.Logger)
	Chrome                 func(config.Config, *slog.Logger) *chromeMode
	Telemetry              func(config.Config, *sqlite.Store, *slog.Logger) (ports.EventSink, ports.TelemetryIdentityStore, bool)
	InitSentry             func(sentryobs.Config) error
	FlushSentry            func(time.Duration)
	IdentityLookup         func(context.Context, config.Config, ports.TelemetryIdentityStore, *slog.Logger)
	StopProviderHost       func(context.Context, string, string) error
	ProviderHostPID        func(string, string) (int, bool)
	ReconcileProviderHosts func(context.Context, string, *sqlite.Store, string) error
	ComputerDriver         func() computeruse.Driver
	ContainerReaper        func() ports.ContainerReaper
	Mobile                 func(config.Config, *controllers.BridgeService, *systeminstall.Service, string, *slog.Logger) mobileBootEffects
	MobileLAN              func(http.Handler, string, int, *slog.Logger, ports.EventSink) bootMobileLAN
	AwakeHost              func(int, string, string) bootAwakeHost
	ShutdownContext        func() (context.Context, context.CancelFunc, context.CancelFunc)
	Runtime                func(*slog.Logger, string) runtimeselect.Runtime
	AgentResolver          func(string, *slog.Logger) (ports.AgentResolver, error)
	AccountLimitsInterval  func() time.Duration
	AgentServices          func(context.Context, config.Config, *sqlite.Store, ports.CodexOperationGate, *slog.Logger) (agentBootComponents, error)
	ChatDrivers            func(*slog.Logger, func()) *chatdriverregistry.Registry
	Workspace              func(config.Config, *sqlite.Store, *slog.Logger) (ports.Workspace, error)
	Reviewers              func() (bootReviewerResolver, error)
	Tracker                func(config.GitLabConfig, *slog.Logger) ports.Tracker
	SCM                    func(config.GitLabConfig, *slog.Logger) *scmmulti.Provider
	Merger                 func(config.GitLabConfig, *slog.Logger) *scmmulti.Merger
	GitHubRepos            func(*slog.Logger) ports.GitHubRepositoryResolver
}

// validate intentionally examines every field. Adding a dependency cannot
// silently make the fixture inherit a native production fallback.
func (d bootDependencies) validate() error {
	return validateBootFunctions("daemon", d)
}

func validateBootFunctions(scope string, dependencies any) error {
	value := reflect.ValueOf(dependencies)
	kind := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() != reflect.Func {
			return fmt.Errorf("%s boot dependency %s has no completeness rule", scope, kind.Field(i).Name)
		}
		if field.IsNil() {
			return fmt.Errorf("%s boot dependency %s is required", scope, kind.Field(i).Name)
		}
	}
	return nil
}

// Keep this factory declarative: constructing a dependency set must not start
// a provider, inspect a native account, spawn a tool, or perform network I/O.
func productionBootDependencies() bootDependencies {
	return bootDependencies{
		HTTPMiddleware:      func(handler http.Handler) http.Handler { return handler },
		FileSnapshotCapture: productionFileSnapshotCapture,
		LegacyImport:        func(store *sqlite.Store) importsvc.Service { return importsvc.New(importsvc.Deps{Store: store}) },
		Observe:             func(bootEvent) {},
		LoadConfig:          config.Load,
		HostCommands:        func(dataDir string) bootHostCommands { return systemexec.New(dataDir) },
		Installer:           productionInstaller,
		Preview:             productionPreview,
		UsagePricing:        newUsagePricingRuntime,
		UsageRoots:          usagesvc.DefaultSourceRoots,
		SessionSearch:       productionSessionSearch,
		Reliability:         startReliability,
		SessionImport: func(store *sqlite.Store, dataDir string, supports func(domain.AgentHarness) bool) (*sessionimport.Service, error) {
			return sessionimport.New(store, dataDir, supports), nil
		},
		LinkPreview: func() *linkpreviewsvc.Service { return linkpreviewsvc.New(nil) },
		GitHubStatus: func(dataDir string, provider *scmmulti.Provider) controllers.GitHubPATService {
			return newGitHubStatusService(githubpat.New(dataDir), provider)
		},
		BranchStateGit:         productionBranchStateGit,
		SCMRepositoryDiscovery: productionSCMRepositoryDiscovery,
		ReapForkSeeds:          reapForkSeeds,
		Chrome:                 newChromeMode,
		Telemetry:              newTelemetrySink,
		InitSentry:             sentryobs.Init,
		FlushSentry:            sentryobs.Flush,
		IdentityLookup:         startGitHubLoginResolver,
		StopProviderHost:       persistenthost.Shutdown,
		ProviderHostPID:        persistenthost.HostPID,
		ReconcileProviderHosts: func(ctx context.Context, dir string, store *sqlite.Store, runID string) error {
			return reconcilePersistentChatHosts(ctx, dir, store, runID)
		},
		ComputerDriver:  func() computeruse.Driver { return cuadriver.New() },
		ContainerReaper: func() ports.ContainerReaper { return dockerreap.New() },
		Mobile:          productionMobileBootEffects,
		MobileLAN: func(handler http.Handler, hostID string, port int, log *slog.Logger, sink ports.EventSink) bootMobileLAN {
			return httpd.NewMobileLAN(handler, hostID, port, log, sink)
		},
		AwakeHost:       func(pid int, binary, dataDir string) bootAwakeHost { return awakesvc.NewHost(pid, binary, dataDir) },
		ShutdownContext: daemonShutdownContext,
		Runtime:         runtimeselect.New,
		AgentResolver: func(harness string, log *slog.Logger) (ports.AgentResolver, error) {
			return buildAgentResolver(harness, log)
		},
		AccountLimitsInterval: func() time.Duration { return time.Minute },
		AgentServices:         productionAgentBoot,
		ChatDrivers:           chatdriverregistry.Build,
		Workspace:             productionWorkspace,
		Reviewers:             func() (bootReviewerResolver, error) { return reviewer.NewResolver() },
		Tracker:               func(cfg config.GitLabConfig, log *slog.Logger) ports.Tracker { return newMultiTracker(cfg, log) },
		SCM:                   newMultiSCMProvider,
		Merger:                newMultiSCMMerger,
		GitHubRepos:           newGitHubRepoResolver,
	}
}
func bootValueMissing(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
