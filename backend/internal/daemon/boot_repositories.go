package daemon

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/reviewer"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	scratchworkspace "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/scratch"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/filesnapshot"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// bootReviewerResolver includes the cache invalidation capability shared by
// native reviewers. Boot accepts an explicit resolver so fixture reviewers can
// never fall back to a production registry.
type bootReviewerResolver interface {
	ports.ReviewerResolver
	harnessBinaryCache
}

func productionReviewers() (bootReviewerResolver, error) {
	return reviewer.NewResolver()
}

func productionWorkspace(cfg config.Config, store *sqlite.Store, log *slog.Logger) (ports.Workspace, error) {
	gitWS, err := gitworktree.New(gitworktree.Options{
		ManagedRoot:  filepath.Join(cfg.DataDir, "worktrees"),
		RepoResolver: projectRepoResolver{store: store},
		Logger:       log,
	})
	if err != nil {
		return nil, fmt.Errorf("session workspace: %w", err)
	}
	scratchWS, err := scratchworkspace.New(scratchworkspace.Options{
		ManagedRoot: filepath.Join(cfg.DataDir, "worktrees"),
	})
	if err != nil {
		return nil, fmt.Errorf("scratch session workspace: %w", err)
	}
	return workspacerouter.New(workspacerouter.Deps{Git: gitWS, Scratch: scratchWS, Projects: store}), nil
}

func productionBranchStateGit() sessionsvc.BranchStateGit {
	return sessionsvc.NewBranchStateGit()
}

func productionSCMRepositoryDiscovery() scmobserve.RepositoryDiscovery {
	return scmobserve.NewRepositoryDiscovery()
}

func productionFileSnapshotCapture() filesnapshot.CaptureFunc {
	return filesnapshot.Capture
}
