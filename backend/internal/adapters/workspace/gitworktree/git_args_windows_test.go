//go:build windows

package gitworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestWindowsGitLongPathsProvisioningPreservesDirtyWork(t *testing.T) {
	// Repository provisioning must never change a user's global Git config.
	globalConfig := filepath.Join(t.TempDir(), "empty-global-config")
	if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := requireGit(t)
	repo := setupOriginClone(t, git, t.TempDir())
	runGit(t, git, repo, "config", "--local", "core.longpaths", "false")
	ctx := context.Background()
	out, err := runCommand(ctx, git, "-C", repo, "config", "--get", "core.longpaths")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("adapter Git longpaths=%q err=%v", out, err)
	}
	out, err = exec.Command(git, "-C", repo, "config", "--local", "--get", "core.longpaths").Output()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("user config changed=%q err=%v", out, err)
	}
	rel := filepath.Join(strings.Repeat("nested-a", 12), strings.Repeat("nested-b", 12), "fixture.txt")
	full := filepath.Join(repo, rel)
	if len(full) <= 260 {
		t.Fatalf("fixture must exceed MAX_PATH, got %d", len(full))
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("long path fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(ctx, git, "-C", repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(ctx, git, "-C", repo, "-c", "user.name=Offline Test", "-c", "user.email=offline@example.invalid", "commit", "-m", "long-path fixture"); err != nil {
		t.Fatal(err)
	}
	head, err := runCommand(ctx, git, "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := New(Options{Binary: git, ManagedRoot: filepath.Join(t.TempDir(), "managed"), RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "long-path-worker", Branch: "ao/long-path", FreshBranch: true, StartRef: strings.TrimSpace(string(head))})
	if err != nil {
		t.Fatal(err)
	}
	local, err := exec.Command(git, "-C", repo, "config", "--local", "--get", "core.longpaths").Output()
	if err != nil || strings.TrimSpace(string(local)) != "true" {
		t.Fatalf("provisioning did not persist required local longpaths: %q %v", local, err)
	}
	global, err := os.ReadFile(globalConfig)
	if err != nil || len(global) != 0 {
		t.Fatalf("global configuration changed: %v", err)
	}
	target := filepath.Join(info.Path, rel)
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "long path fixture\n" {
		t.Fatalf("checkout long file: %q %v", got, err)
	}
	if err := os.WriteFile(target, []byte("dirty content preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ws.Destroy(ctx, info); err == nil {
		t.Fatal("dirty long-path checkout was deleted")
	}
	got, err = os.ReadFile(target)
	if err != nil || string(got) != "dirty content preserved\n" {
		t.Fatalf("dirty content changed: %q %v", got, err)
	}
}
