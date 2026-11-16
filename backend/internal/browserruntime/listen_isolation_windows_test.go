//go:build windows

package browserruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsPipeNamesSeparateEqualBasenames(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "first", "state", "running.json"),
		filepath.Join(root, "second", "state", "running.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	first, err := pipeNameFromRunFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeNameFromRunFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("independent state roots share one pipe: %s", first)
	}
}
