//go:build windows

package sessionmanager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func admissionJunction(t *testing.T, link, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/d", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create isolated junction: %v %s", err, out)
	}
}

func TestAdmissionNativeWindowsPhysicalWorkspaceOwnership(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	owner, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(owner.Metadata.WorkspacePath, "owner-dirty.txt")
	content := []byte("native junction ownership must preserve this work")
	if err := os.WriteFile(dirty, content, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	admissionJunction(t, alias, owner.Metadata.WorkspacePath)
	other, _, release, err := f.manager.reserveSessionAdmission(ctx, "blue", domain.KindWorker, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, path := range []string{alias, strings.ToUpper(owner.Metadata.WorkspacePath)} {
		if err := f.manager.reserveWorkspaceOwner(other, "other", path); !errors.Is(err, ErrWorkspaceOwned) {
			t.Fatalf("alias %q escaped owner fence: %v", path, err)
		}
	}
	mtechAssertDirtyContent(t, dirty, content)
	// A future workspace under a junction must reserve the same physical name
	// even before its leaf directory has been created.
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	futureAlias := filepath.Join(root, "alias")
	admissionJunction(t, futureAlias, physical)
	if err := f.manager.reserveWorkspaceOwner(other, "other", filepath.Join(physical, "future")); err != nil {
		t.Fatal(err)
	}
	third, _, releaseThird, err := f.manager.reserveSessionAdmission(ctx, "green", domain.KindWorker, "third")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseThird()
	if err := f.manager.reserveWorkspaceOwner(third, "third", filepath.Join(futureAlias, "future")); !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatal("missing leaf through junction escaped reservation", err)
	}
	if f.ws.forceCalls.Load() != 0 {
		t.Fatal("ownership check attempted destructive cleanup")
	}
}

func TestAdmissionNativeWindowsRestoreRejectsJunctionBeforeWorkspaceMutation(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	ctx := context.Background()
	owner, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
	if err != nil {
		t.Fatal(err)
	}
	other, _, _, err := f.manager.Spawn(ctx, admissionSpawn("blue"))
	if err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(owner.Metadata.WorkspacePath, "owner-dirty.txt")
	if err := os.WriteFile(dirty, []byte("original writer remains active"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Kill(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "restore-alias")
	admissionJunction(t, alias, owner.Metadata.WorkspacePath)
	saved, found, err := f.store.GetSession(ctx, other.ID)
	if err != nil || !found {
		t.Fatal(err)
	}
	saved.Metadata.WorkspacePath = alias
	if err := f.store.UpdateSession(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.RestoreWithMode(ctx, other.ID); !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatal("restore reached workspace mutation before owner refusal", err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatal(live)
	}
	mtechAssertDirtyContent(t, dirty, []byte("original writer remains active"))
}
