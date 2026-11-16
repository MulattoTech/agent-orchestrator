package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/forkseed"
	scratchworkspace "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/scratch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/filesnapshot"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// All factories in this file are test-only. No missing collaborator or
// unsupported operation selects a real Git, provider, credential, or HTTP path.
func repositoryFixtureEffects(effects bootFixtureEffects) bootFixtureEffects {
	if effects == nil {
		panic("repository fixture: effect recorder is required")
	}
	return effects
}

var fixtureWorkspaceSegment = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")

type bootFixtureWorkspace struct {
	effects bootFixtureEffects
	scratch *scratchworkspace.Workspace
	initErr error
}

func fixtureWorkspace(root string, effects bootFixtureEffects) ports.Workspace {
	effects = repositoryFixtureEffects(effects)
	w := &bootFixtureWorkspace{effects: effects}
	if !filepath.IsAbs(root) {
		w.initErr = effects.Deny("workspace.root.not-absolute")
		return w
	}
	info, err := os.Lstat(root)
	resolved, resolveErr := filepath.EvalSymlinks(root)
	if err != nil || !info.IsDir() || resolveErr != nil || !strings.EqualFold(filepath.Clean(root), filepath.Clean(resolved)) {
		w.initErr = effects.Deny("workspace.root.not-existing-physical-directory")
		return w
	}
	w.scratch, err = scratchworkspace.New(scratchworkspace.Options{ManagedRoot: root})
	if err != nil {
		w.initErr = effects.Deny("workspace.root.initialization-failed")
	}
	return w
}

func (w *bootFixtureWorkspace) config(ctx context.Context, cfg ports.WorkspaceConfig) error {
	if w.initErr != nil {
		return w.initErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	valid := func(id string) bool {
		return fixtureWorkspaceSegment.MatchString(id) && !strings.HasSuffix(id, ".") && id != "." && id != ".."
	}
	if !valid(string(cfg.SessionID)) || (cfg.ProjectID != "" && !valid(string(cfg.ProjectID))) {
		return w.effects.Deny("workspace.identifier.unsafe")
	}
	if cfg.RepoPath != "" || cfg.StartRef != "" || cfg.StartPoint != "" || cfg.BaseRef != "" {
		return w.effects.Deny("workspace.git-configuration.unsupported")
	}
	return nil
}

func (w *bootFixtureWorkspace) path(ctx context.Context, info ports.WorkspaceInfo) error {
	if w.initErr != nil {
		return w.initErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := w.scratch.ObserveWorkspace(ctx, info); err != nil {
		return errors.Join(w.effects.Deny("workspace.path.outside-owned-root"), err)
	}
	return nil
}
func (w *bootFixtureWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := w.config(ctx, cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	// Native scratch routing derives the path from validated project/session
	// components; a supplied arbitrary path is a restore-only capability.
	if cfg.Path != "" {
		return ports.WorkspaceInfo{}, w.effects.Deny("workspace.create.explicit-path")
	}
	w.effects.Allow("workspace.create")
	return w.scratch.Create(ctx, cfg)
}
func (w *bootFixtureWorkspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := w.config(ctx, cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if cfg.Path != "" {
		if err := w.path(ctx, ports.WorkspaceInfo{Path: cfg.Path}); err != nil {
			return ports.WorkspaceInfo{}, err
		}
	}
	w.effects.Allow("workspace.restore")
	return w.scratch.Restore(ctx, cfg)
}
func (w *bootFixtureWorkspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	_, err := w.DestroyReclaim(ctx, info)
	return err
}
func (w *bootFixtureWorkspace) DestroyReclaim(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceReclaim, error) {
	if err := w.path(ctx, info); err != nil {
		return "", err
	}
	w.effects.Allow("workspace.destroy-empty-only")
	return w.scratch.DestroyReclaim(ctx, info)
}
func (w *bootFixtureWorkspace) ForceDestroy(context.Context, ports.WorkspaceInfo) error {
	return w.effects.Deny("workspace.force-destroy")
}
func (w *bootFixtureWorkspace) StashUncommitted(ctx context.Context, info ports.WorkspaceInfo) (string, error) {
	if err := w.path(ctx, info); err != nil {
		return "", err
	}
	w.effects.Allow("workspace.scratch-preserve")
	return "", nil
}
func (w *bootFixtureWorkspace) ApplyPreserved(ctx context.Context, info ports.WorkspaceInfo, ref string) error {
	if err := w.path(ctx, info); err != nil {
		return err
	}
	if ref != "" {
		return w.effects.Deny("workspace.apply-git-ref")
	}
	w.effects.Allow("workspace.scratch-apply-empty")
	return nil
}
func (w *bootFixtureWorkspace) AddExclude(ctx context.Context, info ports.WorkspaceInfo, _ ...string) error {
	if err := w.path(ctx, info); err != nil {
		return err
	}
	w.effects.Allow("workspace.scratch-exclude")
	return nil
}
func (w *bootFixtureWorkspace) ObserveWorkspace(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	if err := w.path(ctx, info); err != nil {
		return ports.WorkspaceObservation{}, err
	}
	w.effects.Allow("workspace.observe")
	return w.scratch.ObserveWorkspace(ctx, info)
}

type bootFixtureReviewers struct{ effects bootFixtureEffects }

func fixtureReviewers(effects bootFixtureEffects) bootReviewerResolver {
	return &bootFixtureReviewers{effects: repositoryFixtureEffects(effects)}
}
func (r *bootFixtureReviewers) Reviewer(harness domain.ReviewerHarness) (ports.Reviewer, bool) {
	if harness != domain.ReviewerCodex {
		_ = r.effects.Deny("reviewer.resolve.unconfigured")
		return nil, false
	}
	r.effects.Allow("reviewer.resolve.fake-codex")
	return r, true
}
func (r *bootFixtureReviewers) InvalidateBinaryResolution(domain.AgentHarness) {
	r.effects.Allow("reviewer.invalidate.fake-cache")
}
func (r *bootFixtureReviewers) ReviewChatHarness() domain.AgentHarness { return domain.HarnessCodex }
func (r *bootFixtureReviewers) ReviewCommand(context.Context, ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	return ports.ReviewCommandSpec{}, r.effects.Deny("reviewer.native-command")
}
func (r *bootFixtureReviewers) ReviewRestoreCommand(context.Context, ports.ReviewInvocation) (ports.ReviewCommandSpec, bool, error) {
	return ports.ReviewCommandSpec{}, false, r.effects.Deny("reviewer.native-restore")
}
func (r *bootFixtureReviewers) ReviewMessage(context.Context, ports.ReviewInvocation) (string, error) {
	return "", r.effects.Deny("reviewer.unconfigured-review")
}
func (r *bootFixtureReviewers) ReviewCancel(context.Context) (ports.ReviewCancelSpec, error) {
	return ports.ReviewCancelSpec{}, r.effects.Deny("reviewer.unconfigured-cancel")
}

type bootFixtureSCM struct{ effects bootFixtureEffects }

func fixtureSCM(effects bootFixtureEffects) *scmmulti.Provider {
	p := &bootFixtureSCM{effects: repositoryFixtureEffects(effects)}
	return scmmulti.New(scmmulti.NamedProvider{Key: "fixture", Provider: p}, scmmulti.NamedProvider{Key: "github", Provider: p}, scmmulti.NamedProvider{Key: "gitlab", Provider: p})
}
func (p *bootFixtureSCM) ParseRepository(remote string) (ports.SCMRepo, bool) {
	if strings.TrimSpace(remote) == "" {
		p.effects.Allow("scm.parse.empty")
	} else {
		_ = p.effects.Deny("scm.repository.unconfigured")
	}
	return ports.SCMRepo{}, false
}
func (p *bootFixtureSCM) SCMCredentialsAvailable(context.Context) (bool, error) {
	p.effects.Allow("scm.credentials.synthetic-unavailable")
	return false, nil
}
func (p *bootFixtureSCM) AuthenticatedIdentity(context.Context) (ports.SCMIdentity, error) {
	return ports.SCMIdentity{}, p.effects.Deny("scm.identity.unconfigured")
}
func (p *bootFixtureSCM) RepoPRListGuard(context.Context, ports.SCMRepo, string) (ports.SCMGuardResult, error) {
	return ports.SCMGuardResult{}, p.effects.Deny("scm.repo-guard.unconfigured")
}
func (p *bootFixtureSCM) ListPRsByRepo(context.Context, ports.SCMRepo, time.Time) ([]ports.SCMPRObservation, error) {
	return nil, p.effects.Deny("scm.list-prs.unconfigured")
}
func (p *bootFixtureSCM) CommitChecksGuard(context.Context, ports.SCMRepo, string, string) (ports.SCMGuardResult, error) {
	return ports.SCMGuardResult{}, p.effects.Deny("scm.commit-guard.unconfigured")
}
func (p *bootFixtureSCM) FetchPullRequests(context.Context, []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	return nil, p.effects.Deny("scm.fetch-prs.unconfigured")
}
func (p *bootFixtureSCM) FetchFailedCheckLogTail(context.Context, ports.SCMRepo, ports.SCMCheckObservation) (string, error) {
	return "", p.effects.Deny("scm.check-log.unconfigured")
}
func (p *bootFixtureSCM) FetchReviewThreads(context.Context, ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	return ports.SCMReviewObservation{}, p.effects.Deny("scm.review-threads.unconfigured")
}
func (p *bootFixtureSCM) RequestReview(context.Context, ports.SCMReviewRequest) error {
	return p.effects.Deny("scm.request-review")
}
func (p *bootFixtureSCM) ResolveReviewThread(context.Context, ports.SCMReviewResolveRequest) error {
	return p.effects.Deny("scm.resolve-review")
}
func (p *bootFixtureSCM) PublishReview(context.Context, ports.SCMReviewPublication) (ports.SCMReviewPublicationReceipt, error) {
	return ports.SCMReviewPublicationReceipt{}, p.effects.Deny("scm.publish-review")
}
func (p *bootFixtureSCM) ChangePullRequestState(context.Context, ports.SCMPRStateRequest) error {
	return p.effects.Deny("scm.change-pr-state")
}
func (p *bootFixtureSCM) PostComment(context.Context, ports.SCMCommentRequest) (ports.SCMCommentResult, error) {
	return ports.SCMCommentResult{}, p.effects.Deny("scm.post-comment")
}

type bootFixtureMerger struct{ effects bootFixtureEffects }

func fixtureMerger(effects bootFixtureEffects) *scmmulti.Merger {
	p := &bootFixtureMerger{effects: repositoryFixtureEffects(effects)}
	return scmmulti.NewMerger(scmmulti.NamedMerger{Key: "fixture", Merger: p}, scmmulti.NamedMerger{Key: "github", Merger: p}, scmmulti.NamedMerger{Key: "gitlab", Merger: p})
}
func (p *bootFixtureMerger) MergePullRequest(context.Context, ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	return ports.SCMMergeResult{}, p.effects.Deny("scm.merge")
}

type bootFixtureTracker struct{ effects bootFixtureEffects }

func fixtureTracker(effects bootFixtureEffects) ports.Tracker {
	return &bootFixtureTracker{effects: repositoryFixtureEffects(effects)}
}
func (p *bootFixtureTracker) Preflight(context.Context) error {
	p.effects.Allow("tracker.preflight.synthetic")
	return nil
}
func (p *bootFixtureTracker) Get(context.Context, domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, p.effects.Deny("tracker.get.unconfigured")
}
func (p *bootFixtureTracker) List(context.Context, domain.TrackerRepo, domain.ListFilter) ([]domain.Issue, error) {
	return nil, p.effects.Deny("tracker.list.unconfigured")
}

type bootFixtureGitHubRepos struct{ effects bootFixtureEffects }

func fixtureGitHubRepos(effects bootFixtureEffects) ports.GitHubRepositoryResolver {
	return &bootFixtureGitHubRepos{effects: repositoryFixtureEffects(effects)}
}
func (p *bootFixtureGitHubRepos) ResolveGitHubRepository(context.Context, string, string) (ports.GitHubRepository, error) {
	return ports.GitHubRepository{}, p.effects.Deny("github.repository.unconfigured")
}

type bootFixtureBranchGit struct{ effects bootFixtureEffects }

func fixtureBranchStateGit(effects bootFixtureEffects) sessionsvc.BranchStateGit {
	return &bootFixtureBranchGit{effects: repositoryFixtureEffects(effects)}
}
func (p *bootFixtureBranchGit) Output(context.Context, string, ...string) (string, error) {
	return "", p.effects.Deny("git.branch-output")
}
func (p *bootFixtureBranchGit) MergeConflictFiles(context.Context, string, string, string) ([]string, bool) {
	_ = p.effects.Deny("git.merge-conflict")
	return nil, false
}

type bootFixtureRepositoryDiscovery struct{ effects bootFixtureEffects }

func fixtureRepositoryDiscovery(effects bootFixtureEffects) scmobserve.RepositoryDiscovery {
	return &bootFixtureRepositoryDiscovery{effects: repositoryFixtureEffects(effects)}
}
func (p *bootFixtureRepositoryDiscovery) OriginURL(context.Context, string) string {
	_ = p.effects.Deny("git.origin-url")
	return ""
}
func (p *bootFixtureRepositoryDiscovery) RemoteURLs(context.Context, string) []string {
	_ = p.effects.Deny("git.remote-urls")
	return nil
}

// fixtureReapForkSeeds keeps both real recovery layers. The synthetic runner
// knows exactly one owned old ref and persists its removal for this fixture's
// lifetime. It cannot execute Git or accept another command/ref.
func fixtureReapForkSeeds(effects bootFixtureEffects) func(context.Context, *sqlite.Store, *slog.Logger) {
	effects = repositoryFixtureEffects(effects)
	var mu sync.Mutex
	removed := map[string]bool{}
	ref := forkseed.RefPrefix + "fixture-orphan"
	runner := func(ctx context.Context, root string, env []string, args ...string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		mu.Lock()
		defer mu.Unlock()
		key := filepath.Clean(root)
		joined := strings.Join(args, "\x00")
		if len(env) != 0 {
			return "", effects.Deny("fork-seed.unconfigured-env")
		}
		switch joined {
		case "for-each-ref\x00--format=%(refname) %(committerdate:unix)\x00" + forkseed.RefPrefix:
			effects.Allow("fork-seed.synthetic-list")
			if removed[key] {
				return "", nil
			}
			return ref + " 1", nil
		case "rev-parse\x00--verify\x00--quiet\x00" + ref:
			effects.Allow("fork-seed.synthetic-exists")
			if removed[key] {
				return "", fmt.Errorf("synthetic ref absent")
			}
			return strings.Repeat("a", 40), nil
		case "update-ref\x00-d\x00" + ref:
			effects.Allow("fork-seed.synthetic-delete")
			removed[key] = true
			return "", nil
		default:
			return "", effects.Deny("fork-seed.unconfigured-command")
		}
	}
	return func(ctx context.Context, store *sqlite.Store, log *slog.Logger) {
		effects.Allow("fork-seed.reconcile")
		err := reapForkSeedsWithReaper(ctx, store, log, func(ctx context.Context, root string, age time.Duration, now time.Time) (int, error) {
			return forkseed.ReapWithRunner(ctx, root, age, now, runner)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = effects.Deny("fork-seed.reconcile-failed")
		}
	}
}

// fixtureFileSnapshotCapture retains the real capture algorithm and makes its
// command boundary explicit. Scratch dispatch must bypass it; an unexpected Git
// project is a counted denial and cannot discover or launch a native Git binary.
func fixtureFileSnapshotCapture(effects bootFixtureEffects) filesnapshot.CaptureFunc {
	effects = repositoryFixtureEffects(effects)
	return func(ctx context.Context, workspace string, refs ...string) error {
		return filesnapshot.CaptureWithRunner(ctx, workspace,
			func(context.Context, string, []string, io.Reader, ...string) (string, error) {
				return "", effects.Deny("git.file-snapshot")
			}, refs...)
	}
}
