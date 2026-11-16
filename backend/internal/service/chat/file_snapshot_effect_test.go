package chat_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/filesnapshot"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The project record is authoritative. A .git directory appearing in Scratch
// never turns ordinary Scratch dispatch into an executable Git operation.
func TestScratchDispatchSkipsGitSnapshotEvenWithGitDirectory(t *testing.T) {
	for _, withGit := range []bool{false, true} {
		t.Run(fmt.Sprintf("git_directory_%v", withGit), func(t *testing.T) {
			t.Setenv("PATH", "")
			chatsvc.EnableForkSnapshots(true)
			var artifacts, captures atomic.Int32
			chatsvc.SetTurnArtifactSnapshot(func(context.Context, domain.SessionID, []string) {
				artifacts.Add(1)
			})
			t.Cleanup(func() {
				chatsvc.EnableForkSnapshots(false)
				chatsvc.SetTurnArtifactSnapshot(nil)
			})
			workspace := t.TempDir()
			if withGit {
				if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(workspace, ".git", "HEAD"), []byte("synthetic, not a repository"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dirty := filepath.Join(workspace, "dirty.txt")
			if err := os.WriteFile(dirty, []byte("preserve scratch content"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := newSnapshotEffectHarness(t, workspace, domain.ProjectKindScratch, func(context.Context, string, ...string) error {
				captures.Add(1)
				return fmt.Errorf("unexpected Git snapshot")
			})
			completeTurn(t, h, "synthetic scratch turn", "provider-turn-1")
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 2 })
			if captures.Load() != 0 || artifacts.Load() != 1 {
				t.Fatalf("Git capture calls=%d, artifact snapshots=%d; want 0, 1", captures.Load(), artifacts.Load())
			}
			if got, err := os.ReadFile(dirty); err != nil || string(got) != "preserve scratch content" {
				t.Fatalf("dirty scratch content = %q, %v", got, err)
			}
		})
	}
}

func TestGitProjectDispatchUsesOnlyItsInjectedSnapshotEffect(t *testing.T) {
	t.Setenv("PATH", "")
	chatsvc.EnableForkSnapshots(true)
	t.Cleanup(func() { chatsvc.EnableForkSnapshots(false) })
	workspace := t.TempDir()
	var captures atomic.Int32
	var gotWorkspace string
	var gotRefs []string
	h := newSnapshotEffectHarness(t, workspace, domain.ProjectKindSingleRepo, func(_ context.Context, path string, refs ...string) error {
		captures.Add(1)
		gotWorkspace, gotRefs = path, append([]string(nil), refs...)
		return filesnapshot.ErrNotRepository
	})
	turnID := completeTurn(t, h, "synthetic Git-project turn", "provider-turn-1")
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 2 })
	if captures.Load() != 1 || gotWorkspace != workspace || len(gotRefs) != 1 || gotRefs[0] != filesnapshot.Ref(testSession, turnID) {
		t.Fatalf("capture calls=%d, workspace=%q, refs=%v", captures.Load(), gotWorkspace, gotRefs)
	}
}

func newSnapshotEffectHarness(t *testing.T, workspace string, kind domain.ProjectKind, capture filesnapshot.CaptureFunc) *harness {
	t.Helper()
	st := openStore(t)
	ctx := context.Background()
	project, found, err := st.GetProject(ctx, string(testProject))
	if err != nil || !found {
		t.Fatalf("read project: found=%v, err=%v", found, err)
	}
	project.Kind = kind
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	record, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("read session: found=%v, err=%v", found, err)
	}
	record.Metadata.WorkspacePath = workspace
	if err := st.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	conv := newFakeConversation()
	var ids atomic.Int64
	h := &harness{st: st, conv: conv, clock: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	h.svc = chatsvc.New(chatsvc.Options{
		Store: st, Reader: fullSnapshotReader(st), Sessions: st,
		Drivers:             fakeRegistry{driver: fakeDriver{conv: conv}},
		FileSnapshotCapture: capture,
		NewID:               func() string { return fmt.Sprintf("snapshot-effect-%d", ids.Add(1)) },
		Now:                 h.now,
	})
	h.ctrl, err = h.svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.svc.Stop(context.Background(), testSession) })
	return h
}
