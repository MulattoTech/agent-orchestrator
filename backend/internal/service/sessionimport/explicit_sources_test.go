package sessionimport

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExplicitImportEmptySourcesDoNotDiscoverRegisteredProjects(t *testing.T) {
	root := t.TempDir()
	svc, err := NewWithSourceRoots(&importStore{projects: []domain.ProjectRecord{{ID: "outside", Path: filepath.Join(root, "must-not-read")}}}, filepath.Join(root, "data"), root, map[domain.AgentHarness]string{}, chatSupported)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Scan(context.Background())
	if err != nil || len(got.Candidates) != 0 {
		t.Fatalf("scan=%+v,%v", got, err)
	}
	rec := domain.SessionRecord{Harness: domain.HarnessCodex, Metadata: domain.SessionMetadata{ImportSource: &domain.SessionImportSource{ConfigDir: root, NativeID: "outside"}, NativeTranscriptPath: filepath.Join(root, "no-read.jsonl")}}
	if err := svc.RefreshArchive(context.Background(), rec); err == nil || !strings.Contains(err.Error(), "explicitly configured") {
		t.Fatalf("unconfigured archive error=%v", err)
	}
}
func TestExplicitImportArchiveCannotEscapeConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "native")
	store := &importStore{}
	svc, err := NewWithSourceRoots(store, filepath.Join(root, "data"), root, map[domain.AgentHarness]string{domain.HarnessClaudeCode: source}, chatSupported)
	if err != nil {
		t.Fatal(err)
	}
	nativeID := "11111111-1111-4111-8111-111111111111"
	path := writeClaude(t, source, nativeID, "", "synthetic", time.Now())
	rec := domain.SessionRecord{ID: "imported", Harness: domain.HarnessClaudeCode, Metadata: domain.SessionMetadata{ImportSource: &domain.SessionImportSource{ConfigDir: source, NativeID: nativeID}, NativeTranscriptPath: path}}
	if err := svc.RefreshArchive(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(store.messages) != 1 {
		t.Fatal("fixture transcript was not read")
	}
	rec.Metadata.NativeTranscriptPath = filepath.Join(root, "outside.jsonl")
	if err := svc.RefreshArchive(context.Background(), rec); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatalf("outside archive error=%v", err)
	}
	rec.Metadata.NativeTranscriptPath = path
	rec.Metadata.ImportSource.ConfigDir = root
	if err := svc.RefreshArchive(context.Background(), rec); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatalf("outside config error=%v", err)
	}
}
func TestExplicitImportRejectsImplicitHomeAndRegisteredSources(t *testing.T) {
	root := t.TempDir()
	for _, h := range []domain.AgentHarness{conductorSource, domain.HarnessAider, domain.HarnessGemini} {
		if _, err := NewWithSourceRoots(&importStore{}, root, root, map[domain.AgentHarness]string{h: root}, chatSupported); err == nil {
			t.Fatalf("accepted %s implicit roots", h)
		}
	}
	var store *importStore
	if _, err := NewWithSourceRoots(store, root, root, map[domain.AgentHarness]string{}, chatSupported); err == nil {
		t.Fatal("accepted typed-nil store")
	}
}
