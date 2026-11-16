package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type repositoryFixtureContractEffects struct {
	mu      sync.Mutex
	allowed map[string]int
	denied  map[string]int
}

func newRepositoryFixtureContractEffects() *repositoryFixtureContractEffects {
	return &repositoryFixtureContractEffects{allowed: map[string]int{}, denied: map[string]int{}}
}
func (e *repositoryFixtureContractEffects) Allow(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.allowed[name]++
}
func (e *repositoryFixtureContractEffects) Deny(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.denied[name]++
	return fmt.Errorf("fixture effect denied: %s", name)
}

func TestBootFixtureWorkspacePreservesDirtyBytesAndConfinesWrites(t *testing.T) {
	t.Setenv("PATH", "")
	ctx := context.Background()
	effects := newRepositoryFixtureContractEffects()
	root := t.TempDir()
	workspace := fixtureWorkspace(root, effects)
	cfg := ports.WorkspaceConfig{SessionID: "fixture-worker", ProjectID: "scratch", Kind: domain.KindWorker}
	info, err := workspace.Create(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(info.Path, "dirty.txt")
	const body = "fixture dirty bytes must survive\n"
	if err := os.WriteFile(dirty, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Path = info.Path
	restored, err := workspace.Restore(ctx, cfg)
	if err != nil || restored.Path != info.Path {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	if err := workspace.Destroy(ctx, info); !errors.Is(err, ports.ErrWorkspaceDirty) {
		t.Fatalf("nonempty scratch destruction must refuse, got %v", err)
	}
	if err := workspace.ForceDestroy(ctx, info); err == nil {
		t.Fatal("fixture force delete allowed")
	}
	raw, err := os.ReadFile(dirty)
	if err != nil || string(raw) != body {
		t.Fatalf("dirty bytes changed: %q err=%v", raw, err)
	}

	outside := t.TempDir()
	cfg.Path = filepath.Join(outside, "must-not-create")
	if _, err := workspace.Restore(ctx, cfg); err == nil {
		t.Fatal("outside root restore accepted")
	}
	if _, err := os.Stat(cfg.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside path created: %v", err)
	}
	cfg.Path = ""
	cfg.SessionID = "../escape"
	if _, err := workspace.Create(ctx, cfg); err == nil {
		t.Fatal("traversal session id accepted")
	}
	if effects.denied["workspace.force-destroy"] != 1 || effects.denied["workspace.path.outside-owned-root"] != 1 || effects.denied["workspace.identifier.unsafe"] != 1 {
		t.Fatalf("denial evidence missing: %+v", effects.denied)
	}
}

func TestBootFixtureWorkspaceRejectsSymlinkedRestore(t *testing.T) {
	ctx := context.Background()
	effects := newRepositoryFixtureContractEffects()
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	workspace := fixtureWorkspace(root, effects)
	_, err := workspace.Restore(ctx, ports.WorkspaceConfig{SessionID: "fixture-worker", Path: filepath.Join(link, "must-not-create")})
	if err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "must-not-create")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped destination was changed: %v", err)
	}
	if effects.denied["workspace.path.outside-owned-root"] != 1 {
		t.Fatalf("denial not recorded: %+v", effects.denied)
	}
}

func TestBootFixtureRepositoryPortsDenyUnconfiguredEffects(t *testing.T) {
	t.Setenv("PATH", "")
	ctx := context.Background()
	effects := newRepositoryFixtureContractEffects()
	provider := fixtureSCM(effects)
	if _, ok := provider.ParseRepository("https://fixture.invalid/unconfigured/repository"); ok {
		t.Fatal("unconfigured repository resolved")
	}
	if available, err := provider.SCMCredentialsAvailable(ctx); err != nil || available {
		t.Fatalf("synthetic credentials=%v err=%v", available, err)
	}
	pr := ports.SCMPRRef{Repo: ports.SCMRepo{Provider: "fixture"}}
	if _, err := provider.PublishReview(ctx, ports.SCMReviewPublication{PR: pr, AllowCreate: true}); err == nil {
		t.Fatal("review publication accepted")
	}
	if _, err := fixtureMerger(effects).MergePullRequest(ctx, ports.SCMMergeRequest{PR: pr}); err == nil {
		t.Fatal("merge accepted")
	}
	reviewer, ok := fixtureReviewers(effects).Reviewer(domain.ReviewerCodex)
	if !ok {
		t.Fatal("fake reviewer not resolved")
	}
	if _, err := reviewer.ReviewCommand(ctx, ports.ReviewInvocation{}); err == nil {
		t.Fatal("native reviewer command accepted")
	}
	if _, err := reviewer.ReviewMessage(ctx, ports.ReviewInvocation{}); err == nil {
		t.Fatal("unconfigured reviewer request accepted")
	}
	tracker := fixtureTracker(effects)
	if err := tracker.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.Get(ctx, domain.TrackerID{}); err == nil {
		t.Fatal("unconfigured tracker lookup accepted")
	}
	if _, err := fixtureGitHubRepos(effects).ResolveGitHubRepository(ctx, "fixture", "repository"); err == nil {
		t.Fatal("repository network lookup accepted")
	}
	if _, err := fixtureBranchStateGit(effects).Output(ctx, t.TempDir(), "status"); err == nil {
		t.Fatal("native branch Git accepted")
	}
	discovery := fixtureRepositoryDiscovery(effects)
	if discovery.OriginURL(ctx, t.TempDir()) != "" || len(discovery.RemoteURLs(ctx, t.TempDir())) != 0 {
		t.Fatal("native repository facts invented")
	}
	for _, key := range []string{"scm.repository.unconfigured", "scm.publish-review", "scm.merge", "reviewer.native-command", "reviewer.unconfigured-review", "tracker.get.unconfigured", "github.repository.unconfigured", "git.branch-output", "git.origin-url", "git.remote-urls"} {
		if effects.denied[key] == 0 {
			t.Fatalf("unconfigured effect %q was not recorded", key)
		}
	}
}

func TestBootFixtureForkSeedRecoveryRetainsBothRealAlgorithms(t *testing.T) {
	t.Setenv("PATH", "")
	ctx := context.Background()
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, project := range []domain.ProjectRecord{
		{ID: "fixture-repository", Path: t.TempDir(), Kind: domain.ProjectKindSingleRepo},
		{ID: "fixture-scratch", Path: t.TempDir(), Kind: domain.ProjectKindScratch},
	} {
		if err := store.UpsertProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	effects := newRepositoryFixtureContractEffects()
	reap := fixtureReapForkSeeds(effects)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reap(ctx, store, log)
	reap(ctx, store, log)
	want := map[string]int{"fork-seed.reconcile": 2, "fork-seed.synthetic-list": 2, "fork-seed.synthetic-exists": 1, "fork-seed.synthetic-delete": 1}
	for key, count := range want {
		if effects.allowed[key] != count {
			t.Fatalf("effect %s count=%d want=%d", key, effects.allowed[key], count)
		}
	}
	if len(effects.denied) != 0 {
		t.Fatalf("unexpected denied effects: %+v", effects.denied)
	}
}
