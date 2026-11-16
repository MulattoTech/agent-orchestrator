package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCleanupAutomaticRetryBoundedAndScheduled(t *testing.T) {
	m, st, rt, ws := newManager()
	now := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return now }
	rec := mkLive("mer-1")
	rec.Metadata.WorkspacePath = t.TempDir()
	dirty := filepath.Join(rec.Metadata.WorkspacePath, "preserve.txt")
	content := []byte("owned content must survive transient Windows cleanup failures")
	if err := os.WriteFile(dirty, content, 0600); err != nil {
		t.Fatal(err)
	}
	st.sessions[rec.ID] = rec
	ws.destroyErr = ports.ErrWorkspaceDeferred
	if _, err := m.Kill(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	facts, found := cleanupFacts(t, st, rec.ID)
	if !found || facts.WorkspaceDisposition != domain.DispositionPending || facts.AttemptCount != 1 || facts.NextAttemptAt.Before(now.Add(5*time.Second)) || !facts.NextAttemptAt.Before(now.Add(7*time.Second)) {
		t.Fatal(facts, found)
	}
	if ws.destroyed != 1 {
		t.Fatal(ws.destroyed)
	}
	due := facts.NextAttemptAt
	now = now.Add(time.Second)
	if err := m.ReconcileWorkspaceCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 1 {
		t.Fatal("retry happened before due time", ws.destroyed)
	}
	now = due
	if err := m.ReconcileWorkspaceCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	facts, _ = cleanupFacts(t, st, rec.ID)
	if ws.destroyed != 2 || facts.AttemptCount != 2 || facts.WorkspaceDisposition != domain.DispositionFailed || facts.FailureCode != "WORKSPACE_RETRY_EXHAUSTED" || !facts.NextAttemptAt.IsZero() {
		t.Fatal(ws.destroyed, facts)
	}
	// Restart the manager; the durable exhausted result cannot reopen retries.
	m = New(Deps{Runtime: rt, Agents: fakeAgents{}, Workspace: ws, Store: st, Lifecycle: &fakeLCM{store: st}, Clock: func() time.Time { return now }})
	now = now.Add(time.Hour)
	for range 3 {
		if err := m.ReconcileWorkspaceCleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if ws.destroyed != 2 {
		t.Fatal("restart reopened exhausted cleanup", ws.destroyed)
	}
	after, err := os.ReadFile(dirty)
	if err != nil || string(after) != string(content) {
		t.Fatal("work content changed", err)
	}
	// An explicit owner retry remains possible after the external lock is fixed.
	ws.destroyErr = nil
	result, err := m.Cleanup(context.Background(), rec.ProjectID)
	facts, _ = cleanupFacts(t, st, rec.ID)
	if err != nil || len(result.Cleaned) != 1 || ws.destroyed != 3 || facts.AttemptCount != 3 || facts.WorkspaceDisposition != domain.DispositionRemoved {
		t.Fatal(result, err, ws.destroyed, facts)
	}
}

func TestCleanupDeferredCompletionCountsOneAttempt(t *testing.T) {
	m, st, _, _ := newManager()
	rec := mkLive("mer-1")
	rec.IsTerminated = true
	st.sessions[rec.ID] = rec
	if err := m.recordWorkspaceCleanup(context.Background(), rec, domain.DispositionPending, "", false); err != nil {
		t.Fatal(err)
	}
	if err := m.recordWorkspaceCleanupResult(context.Background(), rec, cleanupRefusal{code: domain.CleanupSkipWorktreeBusy}, false); err != nil {
		t.Fatal(err)
	}
	facts, _ := cleanupFacts(t, st, rec.ID)
	if facts.AttemptCount != 1 || facts.WorkspaceDisposition != domain.DispositionPending || facts.NextAttemptAt.IsZero() {
		t.Fatal(facts)
	}
}

func TestCleanupLegacyExhaustedPendingPreservesWorkspace(t *testing.T) {
	m, st, _, ws := newManager()
	path := t.TempDir()
	seedTerminal(st, "mer-1", domain.SessionMetadata{WorkspacePath: path})
	st.cleanupFacts = map[domain.SessionID]domain.SessionCleanupRecord{"mer-1": {SessionID: "mer-1", WorkspaceDisposition: domain.DispositionPending, AttemptCount: 12}}
	if err := m.ReconcileWorkspaceCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	facts, _ := cleanupFacts(t, st, "mer-1")
	if ws.destroyed != 0 || facts.AttemptCount != 12 || facts.WorkspaceDisposition != domain.DispositionFailed || facts.FailureCode != "WORKSPACE_RETRY_EXHAUSTED" {
		t.Fatal(ws.destroyed, facts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("workspace touched", err)
	}
}

func TestCleanupDirtyRefusalDoesNotAutomaticallyRetry(t *testing.T) {
	m, st, _, ws := newManager()
	st.sessions["mer-1"] = mkLive("mer-1")
	ws.destroyErr = ports.ErrWorkspaceDirty
	if _, err := m.Kill(context.Background(), "mer-1"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := m.ReconcileWorkspaceCleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	facts, _ := cleanupFacts(t, st, "mer-1")
	if ws.destroyed != 1 || facts.AttemptCount != 1 || facts.WorkspaceDisposition != domain.DispositionPreservedDirty || !facts.NextAttemptAt.IsZero() {
		t.Fatal(ws.destroyed, facts)
	}
}
