//go:build windows

package localipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPipeNameUsesCompletePhysicalRoot(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first", "state")
	second := filepath.Join(root, "second", "state")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	want, err := PipeName(filepath.Join(first, "running.json"), "supervise")
	if err != nil {
		t.Fatal(err)
	}
	other, err := PipeName(filepath.Join(second, "running.json"), "supervise")
	if err != nil || other == want {
		t.Fatalf("roots collide: %q %q err=%v", want, other, err)
	}
	if !strings.HasPrefix(want, "\\\\.\\pipe\\ao-supervise-v2-") || len(want) > 160 {
		t.Fatalf("unexpected bounded locator %q", want)
	}
	for _, alias := range []string{
		strings.ToUpper(filepath.Join(first, "running.json")),
		filepath.ToSlash(filepath.Join(first, "running.json")),
		first + "\\.\\running.json",
		filepath.Join(first, "ignored.json"),
	} {
		got, err := PipeName(alias, "supervise")
		if err != nil || got != want {
			t.Fatalf("alias %q=%q want%q err=%v", alias, got, want, err)
		}
	}
	seen := map[string]bool{want: true}
	for _, purpose := range []string{"browser-runtime-browser", "browser-runtime-chrome", "browser-runtime-stream", "browser-stream"} {
		got, err := PipeName(filepath.Join(first, "running.json"), purpose)
		if err != nil || seen[got] {
			t.Fatalf("service collision %s=%s err=%v", purpose, got, err)
		}
		seen[got] = true
	}
}

func TestPipeNameRejectsUnqualifiedRootsAndPurposes(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("ordinary fixture file"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "running.json", "C:relative\\running.json", filepath.Join(root, "missing", "running.json"), filepath.Join(file, "running.json")} {
		if got, err := PipeName(path, "supervise"); err == nil || got != "" {
			t.Fatalf("unqualified root %q accepted as %q err=%v", path, got, err)
		}
	}
	for _, purpose := range []string{"", "supervise/other", "supervise\\other", "UPPER", "trailing-", strings.Repeat("x", 49)} {
		if got, err := PipeName(filepath.Join(root, "running.json"), purpose); err == nil || got != "" {
			t.Fatalf("unqualified service %q accepted as %q", purpose, got)
		}
	}
}
