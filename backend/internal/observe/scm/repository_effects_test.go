package scm

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type repositoryEffectsFixture struct {
	originCalls int
	remoteCalls int
	path        string
}

func (f *repositoryEffectsFixture) OriginURL(_ context.Context, path string) string {
	f.originCalls++
	f.path = path
	return "https://github.com/o/r"
}
func (f *repositoryEffectsFixture) RemoteURLs(_ context.Context, path string) []string {
	f.remoteCalls++
	f.path = path
	return []string{"https://github.com/o/r", "https://github.com/o/extra"}
}

func TestExplicitRepositoryDiscoveryBackfillsAndDiscoversWithoutGit(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "worker", ProjectID: "project", Metadata: domain.SessionMetadata{Branch: "ao/worker"}}},
		projects: map[string]domain.ProjectRecord{"project": {ID: "project", Path: root}},
	}
	effects := &repositoryEffectsFixture{}
	observer := New(&fakeProvider{}, store, nil, Config{RepositoryDiscovery: effects})
	subjects, repositories, err := observer.discoverSubjects(context.Background())
	if err != nil || len(subjects) != 0 || len(repositories) != 2 {
		t.Fatalf("subjects=%v repositories=%v err=%v", subjects, repositories, err)
	}
	if effects.originCalls != 1 || effects.remoteCalls != 1 || effects.path != root || store.projects["project"].RepoOriginURL != "https://github.com/o/r" {
		t.Fatalf("effects=%+v project=%+v", effects, store.projects["project"])
	}
	if _, _, err := observer.discoverSubjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects.originCalls != 1 || effects.remoteCalls != 2 {
		t.Fatalf("stored origin not reused: %+v", effects)
	}
}
