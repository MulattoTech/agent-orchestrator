package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type bootRepositoryDenyDiscovery struct{ calls atomic.Int32 }

func (d *bootRepositoryDenyDiscovery) OriginURL(context.Context, string) string {
	d.calls.Add(1)
	return ""
}
func (d *bootRepositoryDenyDiscovery) RemoteURLs(context.Context, string) []string {
	d.calls.Add(1)
	return nil
}

type bootRepositoryFakeProvider struct {
	scmobserve.Provider
	credentials atomic.Int32
}

func (p *bootRepositoryFakeProvider) SCMCredentialsAvailable(context.Context) (bool, error) {
	p.credentials.Add(1)
	return false, nil
}

type bootRepositoryBranchPoll struct {
	once   sync.Once
	polled chan struct{}
}

func (p *bootRepositoryBranchPoll) ReconcileBranchStates(context.Context) error {
	p.once.Do(func() { close(p.polled) })
	return nil
}

func TestExplicitSCMObserverRejectsMissingEffectsBeforeStart(t *testing.T) {
	if done, err := startSCMObserverWithProvider(context.Background(), nil, nil, nil, nil, &bootRepositoryDenyDiscovery{}, nil, slog.Default()); err == nil || done != nil {
		t.Fatalf("missing provider: done=%v err=%v", done, err)
	}
	if done, err := startSCMObserverWithProvider(context.Background(), nil, nil, nil, scmmulti.New(), nil, nil, slog.Default()); err == nil || done != nil {
		t.Fatalf("missing repository effects: done=%v err=%v", done, err)
	}
}

func TestExplicitSCMObserverPollsRealLoopWithFakeProvider(t *testing.T) {
	t.Setenv("PATH", "")
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &bootRepositoryFakeProvider{}
	multi := scmmulti.New(scmmulti.NamedProvider{Key: "fixture", Provider: provider})
	discovery := &bootRepositoryDenyDiscovery{}
	branches := &bootRepositoryBranchPoll{polled: make(chan struct{})}
	done, err := startSCMObserverWithProvider(ctx, store, lifecycle.New(store, nil), branches, multi, discovery, &scmReviewRefresher{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-branches.polled:
	case <-time.After(3 * time.Second):
		t.Fatal("real SCM poll never ran")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("real SCM loop did not drain")
	}
	if provider.credentials.Load() != 1 || discovery.calls.Load() != 0 {
		t.Fatalf("credential calls=%d unexpected repository calls=%d", provider.credentials.Load(), discovery.calls.Load())
	}
}

func TestExplicitForkSeedReaperRetainsStartupProjectFiltering(t *testing.T) {
	if err := reapForkSeedsWithReaper(context.Background(), nil, slog.Default(), nil); err == nil {
		t.Fatal("missing reaper must fail before storage or Git")
	}
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gitRoot := t.TempDir()
	rows := []domain.ProjectRecord{
		{ID: "repository", Path: gitRoot},
		{ID: "workspace", Path: t.TempDir(), Kind: domain.ProjectKindWorkspace},
		{ID: "scratch", Path: t.TempDir(), Kind: domain.ProjectKindScratch},
		{ID: "missing", Path: filepath.Join(t.TempDir(), "absent")},
		{ID: "empty"},
	}
	for _, row := range rows {
		if err := store.UpsertProject(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	err = reapForkSeedsWithReaper(context.Background(), store, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context, root string, maxAge time.Duration, now time.Time) (int, error) {
		calls++
		if ctx.Err() != nil || root != gitRoot || maxAge != forkSeedMaxAge || now.IsZero() {
			t.Fatalf("unexpected reaper context: root=%q maxAge=%v now=%v", root, maxAge, now)
		}
		return 1, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestProductionWorkspaceFactoryKeepsScratchRouting(t *testing.T) {
	t.Setenv("PATH", "")
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace, err := productionWorkspace(config.Config{DataDir: t.TempDir()}, store, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	info, err := workspace.Create(context.Background(), ports.WorkspaceConfig{SessionID: "fixture-worker"})
	if err != nil || info.Path == "" || info.SessionID != "fixture-worker" {
		t.Fatalf("scratch routing info=%+v err=%v", info, err)
	}
}
