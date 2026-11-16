package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type branchStateEffectFixture struct {
	outputs   []string
	conflicts int
	fail      bool
}

func (f *branchStateEffectFixture) Output(_ context.Context, _ string, args ...string) (string, error) {
	f.outputs = append(f.outputs, strings.Join(args, " "))
	if f.fail {
		return "", errors.New("fixture Git unavailable")
	}
	switch args[0] {
	case "for-each-ref":
		return "head refs/heads/feature\nbase refs/remotes/origin/main\nremote refs/remotes/origin/feature", nil
	case "rev-list":
		if args[1] == "--left-right" {
			return "2\t1", nil
		}
		return "1", nil
	}
	return "", errors.New("unexpected fixture Git command")
}
func (f *branchStateEffectFixture) MergeConflictFiles(_ context.Context, _ string, base, head string) ([]string, bool) {
	if base != "base" || head != "head" {
		return nil, false
	}
	f.conflicts++
	return []string{"fixture.go"}, true
}

func branchStateEffectWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBranchStateExplicitEffectsPreserveGroupingAndFingerprint(t *testing.T) {
	t.Setenv("PATH", "")
	root := branchStateEffectWorkspace(t)
	st := &branchStateStore{fakeStore: newFakeStore()}
	for _, id := range []domain.SessionID{"one", "two"} {
		st.sessions[id] = domain.SessionRecord{ID: id, Metadata: domain.SessionMetadata{
			WorkspacePath: root, DiffBaseRef: "refs/remotes/origin/main",
		}}
	}
	effects := &branchStateEffectFixture{}
	svc := NewWithDeps(Deps{Store: st, BranchStateGit: effects})
	if err := svc.ReconcileBranchStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(effects.outputs) != 5 || effects.conflicts != 2 || st.writes != 2 {
		t.Fatalf("first pass commands=%v conflicts=%d writes=%d", effects.outputs, effects.conflicts, st.writes)
	}
	for _, rec := range st.sessions {
		if rec.BranchState == nil || rec.BranchState.Commits != 2 || rec.BranchState.Behind != 1 || rec.BranchState.Unpushed != 1 || !rec.BranchState.Conflicts {
			t.Fatalf("real reconciliation did not persist fake command facts: %+v", rec.BranchState)
		}
	}
	if err := svc.ReconcileBranchStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(effects.outputs) != 6 || effects.conflicts != 2 || st.writes != 2 {
		t.Fatalf("idle pass commands=%v conflicts=%d writes=%d", effects.outputs, effects.conflicts, st.writes)
	}
}

func TestBranchStateExplicitReadFailureDoesNotEraseFacts(t *testing.T) {
	t.Setenv("PATH", "")
	root := branchStateEffectWorkspace(t)
	prior := domain.SessionBranchState{Branch: "feature", Head: "preserved", Commits: 7}
	st := &branchStateStore{fakeStore: newFakeStore()}
	st.sessions["one"] = domain.SessionRecord{ID: "one", BranchState: &prior, Metadata: domain.SessionMetadata{WorkspacePath: root}}
	effects := &branchStateEffectFixture{fail: true}
	svc := NewWithDeps(Deps{Store: st, BranchStateGit: effects})
	if err := svc.ReconcileBranchStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.writes != 0 || *st.sessions["one"].BranchState != prior || len(effects.outputs) != 1 {
		t.Fatalf("failed explicit read mutated facts: writes=%d commands=%v state=%+v", st.writes, effects.outputs, st.sessions["one"].BranchState)
	}
}
