//go:build windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// This regression exercises only real local SQLite allocation and the contained
// scratch adapter. It never constructs boot dependencies, launches a child,
// binds a listener, opens an account client or enters the shared Run lifecycle.
func TestOfflineDaemonSeedUsesAllocatedIdentitiesAndDurableHolds(t *testing.T) {
	root := t.TempDir()
	prepareFixtureDirectories(t, root)
	seed := seedDaemonFixture(t, root)
	if len(seed.QueueTurns) != 38 || len(seed.HibernatedSessions) != 38 {
		t.Fatalf("queued=%d held=%d", len(seed.QueueTurns), len(seed.HibernatedSessions))
	}
	store, err := sqlite.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	records, err := store.ListAllSessions(ctx)
	if err != nil || len(records) != 40 {
		t.Fatalf("sessions=%d err=%v", len(records), err)
	}
	live := map[domain.SessionID]bool{}
	for _, rec := range records {
		conversation, err := store.ConversationForSession(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conversation.Settings.Model != fixtureAgentModel || conversation.Settings.ReasoningEffort != fixtureAgentEffort {
			t.Fatalf("durable resume settings for %s = model %q effort %q", rec.ID, conversation.Settings.Model, conversation.Settings.ReasoningEffort)
		}
		if rec.Metadata.ProviderConversationID != "synthetic-thread-"+string(rec.ID) {
			t.Fatalf("native thread identity does not match allocated session %s", rec.ID)
		}
		if _, err := os.Stat(rec.Metadata.WorkspacePath); err != nil {
			t.Fatal(err)
		}
		if rec.HibernatedAt == nil {
			live[rec.ID] = true
		}
	}
	if len(live) != 2 || !live["fixture-project-2"] || !live["fixture-project-3"] {
		t.Fatalf("active fixture identities=%v", live)
	}
	for _, id := range seed.QueueTurns {
		turn, err := store.TurnByID(ctx, id)
		if err != nil || turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" {
			t.Fatalf("queue %s=%+v err=%v", id, turn, err)
		}
	}
	message, found, err := store.ConversationMessageByClientID(ctx, seed.ReportConversation, seed.ReportClientID)
	if err != nil || !found || message.DeliveryContentJSON != seed.ReportContent {
		t.Fatalf("attached report found=%v err=%v", found, err)
	}
	report, found, err := store.GetReport(ctx, "fixture-report")
	if err != nil || !found || report.SessionID != "fixture-project-1" || report.DeliveryBatchID != seed.ReportBatch {
		t.Fatalf("report=%+v found=%v err=%v", report, found, err)
	}
	admissions, err := store.ListSessionAdmission(ctx)
	if err != nil || len(admissions) != 1 || admissions[0].SessionID != "fixture-project-4" || admissions[0].FailureCode != "LAUNCH_OUTCOME_UNCERTAIN" {
		t.Fatalf("admission=%+v err=%v", admissions, err)
	}
	if !store.LimitHistoryEnabled(ctx) {
		t.Fatal("fake account poller history not enabled")
	}
	for _, id := range []string{"fixture-project-2", "fixture-project-3"} {
		if !fixtureHTTPAllowed("POST", "/api/v1/sessions/"+id+"/conversation/messages") {
			t.Fatalf("allocated fixed identity %s not allowlisted", id)
		}
	}
	if fixtureHTTPAllowed("POST", "/api/v1/sessions/fixture-worker-01/conversation/messages") {
		t.Fatal("obsolete guessed identity remains allowlisted")
	}
}
