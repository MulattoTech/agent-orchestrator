//go:build windows

package conpty

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

type candidateTreeRecord struct {
	Token     string `json:"token"`
	Role      string `json:"role"`
	PID       int    `json:"pid"`
	ParentPID int    `json:"parentPID"`
	Creation  uint64 `json:"creationTicks"`
}

func candidateTreeProcesses(t *testing.T, root, nonce, helper string, rootPID int) []*candidateOwnedProcess {
	t.Helper()
	processes := make([]*candidateOwnedProcess, 0, 2)
	expectedParent := rootPID
	for _, role := range []string{"child", "grandchild"} {
		var record candidateTreeRecord
		candidateWait(t, 5*time.Second, role+" ready", func() bool {
			data, err := os.ReadFile(filepath.Join(root, "tree-"+role+".json"))
			return err == nil && json.Unmarshal(data, &record) == nil
		})
		if record.Token != nonce || record.Role != role || record.PID <= 0 || record.ParentPID != expectedParent || record.Creation == 0 {
			t.Fatal("synthetic descendant ownership record mismatch")
		}
		process := candidateProcess(t, candidateFixtureIdentity{PID: record.PID, Creation: record.Creation}, helper)
		processes = append(processes, process)
		t.Cleanup(func() {
			if err := process.stop(); err != nil {
				t.Errorf("owned descendant cleanup: %v", err)
			}
			process.close()
		})
		expectedParent = record.PID
	}
	return processes
}

func assertCandidateTreeAlive(t *testing.T, processes []*candidateOwnedProcess) {
	t.Helper()
	for _, p := range processes {
		if alive, err := p.alive(); err != nil || !alive {
			t.Fatalf("owned descendant %d did not survive controller loss: %t %v", p.identity.PID, alive, err)
		}
	}
}
func assertCandidateTreeExited(t *testing.T, processes []*candidateOwnedProcess) {
	t.Helper()
	candidateWait(t, 3*time.Second, "all owned descendants exit", func() bool {
		for _, p := range processes {
			alive, err := p.alive()
			if err != nil || alive {
				return false
			}
		}
		return true
	})
}

// This native regression launches only the explicit hashed stdlib helper.
// No AO CLI, shipped provider, shell script or account endpoint is involved.
func TestConPTYOwnedJobContainsDescendants(t *testing.T) {
	helper, digest := os.Getenv("AO_CONPTY_HELPER_EXE"), os.Getenv("AO_CONPTY_HELPER_SHA256")
	if helper == "" && digest == "" {
		t.Skip("explicit stdlib helper and exact hash required")
	}
	if !filepath.IsAbs(helper) || filepath.Base(helper) != "ao-offline-worker.exe" || !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(digest) {
		t.Fatal("invalid explicit helper")
	}
	actual, err := candidateHashFile(helper)
	if err != nil || actual != digest {
		t.Fatal("stdlib helper hash mismatch")
	}
	root, nonce := candidateState(t)
	t.Setenv("AO_ADMISSION_NATIVE_HELPER", "1")
	t.Setenv("AO_ADMISSION_HELPER_TREE", "1")
	t.Setenv("AO_ADMISSION_HELPER_READY", filepath.Join(root, "worker.ready"))
	t.Setenv("AO_ADMISSION_HELPER_TOKEN", nonce)
	conn, err := newConPTY(root, helper, nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	processes := candidateTreeProcesses(t, root, nonce, helper, conn.PID())
	assertCandidateTreeAlive(t, processes)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	assertCandidateTreeExited(t, processes)
	select {
	case <-conn.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("owned root did not exit")
	}
	t.Logf("CONPTY_JOB_TREE ownedDescendants=2 closeReapedTree=true helperSHA256=%s providerCalls=0", digest)
}
