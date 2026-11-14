package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/transferbench"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestUndeliveredResultFailsTheCommand(t *testing.T) {
	sample := filepath.Join("..", "..", "testdata", "sample-run.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--run", sample}, &stdout, &stderr); code != transferbench.ExitPass || stdout.String() != "valid\n" {
		t.Fatalf("validate = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"validate", "--run", sample}, failingWriter{}, &stderr); code != transferbench.ExitIncomparable {
		t.Fatalf("validate with a failing stdout = %d, want %d", code, transferbench.ExitIncomparable)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("could not write the result")) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
