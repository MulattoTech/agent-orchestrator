package filesnapshot

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCaptureWithRunnerKeepsSnapshotAlgorithmAndIndexIsolation(t *testing.T) {
	t.Setenv("PATH", "")
	workspace := t.TempDir()
	index := filepath.Join(workspace, "index")
	const original = "synthetic index bytes"
	if err := os.WriteFile(index, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var tempIndex, updates, cleared string
	treeReads := 0
	runner := func(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
		if dir != workspace {
			t.Fatalf("runner dir = %q, want explicit workspace", dir)
		}
		command := strings.Join(args, " ")
		calls = append(calls, command)
		if len(env) == 1 && strings.HasPrefix(env[0], "GIT_INDEX_FILE=") {
			current := strings.TrimPrefix(env[0], "GIT_INDEX_FILE=")
			if current == index {
				t.Fatal("capture exposed the real index to mutation")
			}
			if tempIndex != "" && current != tempIndex {
				t.Fatal("capture switched temporary indexes")
			}
			tempIndex = current
		}
		switch command {
		case "rev-parse --show-toplevel":
			return workspace, nil
		case "rev-parse --verify HEAD":
			return "head", nil
		case "ls-files -z --others --exclude-standard":
			return "", nil
		case "rev-parse --git-path index":
			return index, nil
		case "write-tree":
			if tempIndex == "" {
				t.Fatal("write-tree has no temporary index")
			}
			treeReads++
			if treeReads == 1 {
				got, err := os.ReadFile(tempIndex)
				if err != nil || string(got) != original {
					t.Fatalf("copied index = %q, %v", got, err)
				}
				if err := os.WriteFile(tempIndex, []byte("temporary mutation"), 0o600); err != nil {
					t.Fatal(err)
				}
				return "index-tree", nil
			}
			return "worktree-tree", nil
		case "ls-files -v -z":
			return "h tracked.txt\x00H unchanged.txt\x00", nil
		case "update-index --no-assume-unchanged -z --stdin":
			body, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			cleared = string(body)
			return "", nil
		case "add --all":
			return "", nil
		case "commit-tree index-tree -p head -m ao file snapshot index":
			if len(env) != 4 {
				t.Fatalf("snapshot identity env = %v", env)
			}
			return "index-commit", nil
		case "commit-tree worktree-tree -p head -p index-commit -m " + snapshotSubject + workspace:
			return "snapshot-commit", nil
		case "update-ref --stdin":
			body, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			updates = string(body)
			return "", nil
		default:
			t.Fatalf("unexpected runner call: %q", command)
			return "", errors.New("unexpected command")
		}
	}
	refs := []string{Ref("fixture", "one"), Ref("fixture", "two")}
	if err := CaptureWithRunner(context.Background(), workspace, runner, refs...); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rev-parse --show-toplevel", "rev-parse --verify HEAD",
		"ls-files -z --others --exclude-standard", "rev-parse --git-path index",
		"write-tree", "ls-files -v -z", "update-index --no-assume-unchanged -z --stdin",
		"add --all", "write-tree",
		"commit-tree index-tree -p head -m ao file snapshot index",
		"commit-tree worktree-tree -p head -p index-commit -m " + snapshotSubject + workspace,
		"update-ref --stdin",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("capture sequence = %v, want %v", calls, want)
	}
	if cleared != "tracked.txt\x00" {
		t.Fatalf("assume-unchanged entries = %q", cleared)
	}
	if want := "update " + refs[0] + " snapshot-commit\nupdate " + refs[1] + " snapshot-commit\n"; updates != want {
		t.Fatalf("ref transaction = %q, want %q", updates, want)
	}
	if got, err := os.ReadFile(index); err != nil || string(got) != original {
		t.Fatalf("real index changed: %q, %v", got, err)
	}
	if _, err := os.Stat(tempIndex); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary index survived capture: %v", err)
	}
}

func TestCaptureWithRunnerFailsClosedWithoutRunnerOrAfterCancellation(t *testing.T) {
	t.Setenv("PATH", "")
	workspace := t.TempDir()
	if err := CaptureWithRunner(context.Background(), workspace, nil, Ref("fixture", "nil")); err == nil {
		t.Fatal("missing runner was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	runner := func(context.Context, string, []string, io.Reader, ...string) (string, error) {
		calls++
		return workspace, nil
	}
	if err := CaptureWithRunner(ctx, workspace, runner, Ref("fixture", "cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture = %v", err)
	}
	if calls != 0 {
		t.Fatalf("cancelled capture called runner %d times", calls)
	}
}

func TestCaptureWithRunnerStopsBeforeRefMutationOnCommandFailure(t *testing.T) {
	t.Setenv("PATH", "")
	workspace := t.TempDir()
	sentinel := errors.New("synthetic command failure")
	var calls []string
	runner := func(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		if len(calls) == 1 {
			return workspace, nil
		}
		return "", sentinel
	}
	err := CaptureWithRunner(context.Background(), workspace, runner, Ref("fixture", "failure"))
	if !errors.Is(err, sentinel) || len(calls) != 2 {
		t.Fatalf("capture = %v, commands = %v", err, calls)
	}
}

func TestCaptureWithRunnerRetainsUntrackedSizeGuard(t *testing.T) {
	t.Setenv("PATH", "")
	workspace := t.TempDir()
	calls := 0
	runner := func(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
		calls++
		switch calls {
		case 1:
			return workspace, nil
		case 2:
			return "head", nil
		case 3:
			return strings.Repeat("absent.txt\x00", maxUntrackedFiles+1), nil
		default:
			t.Fatal("oversize capture progressed past size guard")
			return "", errors.New("unexpected command")
		}
	}
	if err := CaptureWithRunner(context.Background(), workspace, runner, Ref("fixture", "oversize")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize capture = %v", err)
	}
	if calls != 3 {
		t.Fatalf("oversize capture made %d calls", calls)
	}
}

func TestCaptureWithRunnerRejectsParentRepository(t *testing.T) {
	t.Setenv("PATH", "")
	parent := t.TempDir()
	workspace := filepath.Join(parent, "nested")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := func(context.Context, string, []string, io.Reader, ...string) (string, error) {
		calls++
		return parent, nil
	}
	if err := CaptureWithRunner(context.Background(), workspace, runner, Ref("fixture", "nested")); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("nested capture = %v", err)
	}
	if calls != 1 {
		t.Fatalf("nested capture made %d calls", calls)
	}
}
