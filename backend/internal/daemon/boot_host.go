package daemon

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/opencode"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/previewserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionsearch"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// bootHostCommands makes optional adapter capabilities explicit at the
// composition boundary; an injected host cannot quietly fall back to native
// command execution, script download, capability discovery or installation.
type bootHostCommands interface {
	ports.ExecutableFinder
	ports.CommandRunner
	ports.InstallCommandRunner
	ports.InstallScriptRunner
	ports.ManagedBinaryInstaller
	ports.InstallCapabilityProbe
	ports.PathWritableProbe
}

func productionInstaller(cfg config.Config, store *sqlite.Store, agents ports.AgentResolver, commands bootHostCommands) (*systeminstall.Service, error) {
	privateNPMPrefixes := map[systeminstall.Target]string{}
	if prefix, err := opencode.V2NPMPrefix(); err == nil {
		privateNPMPrefixes[systeminstall.TargetOpencodeV2] = prefix
	}
	privateInstallerHomes := map[systeminstall.Target]string{}
	if home, err := opencode.V2InstallerHome(); err == nil {
		privateInstallerHomes[systeminstall.TargetOpencodeV2] = home
	}
	return systeminstall.NewWithDeps(commands, commands, systeminstall.Deps{
		JobStore:                  store,
		Verifier:                  systeminstall.NewVerifier(agents, commands),
		Sessions:                  store,
		Reviewers:                 store,
		DisableUpdateChecks:       !cfg.HarnessUpdateChecks,
		DisableUpdateWhileRunning: !cfg.UpdateWhileRunning,
		UpdateWhenIdle:            cfg.UpdateWhenIdle,
		ManagedRoot:               managedHarnessRoot(cfg),
		Releases:                  store,
		PrivateNPMPrefixes:        privateNPMPrefixes,
		PrivateInstallerHomes:     privateInstallerHomes,
	}), nil
}

func productionPreview(log *slog.Logger, dataDir string) (*previewserver.Manager, error) {
	return previewserver.New(log, dataDir), nil
}

func productionSessionSearch(ctx context.Context, store *sqlite.Store, stateDir string, log *slog.Logger) (*sessionsearch.Service, error) {
	return startSessionSearch(ctx, store, stateDir, log), nil
}
