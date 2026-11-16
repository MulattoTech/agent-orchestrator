package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestReportClaimRecoveryRecognizesOnlyDurableSameProjectAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted bool
	}{
		{"human-piggyback", true}, {"standalone-automation", true}, {"uncertain-write", true},
		{"interrupted-before-write", false}, {"interrupted-after-write", true},
		{"never-appended", false}, {"ordinary-user-prefix", false}, {"cross-project", false},
		{"cancelled", false}, {"malformed-content", false}, {"scalar-content", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, err := sqlite.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			seedProject(t, st, "report-crash")
			rec := sampleRecord("report-crash")
			rec.Mode = domain.SessionModeChat
			session, err := st.CreateSession(ctx, rec)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
			original := domain.ReportRecord{
				ID: "report-claim", ProjectID: "report-crash", SessionID: session.ID, State: domain.ReportCheckpoint,
				Note: "immutable report payload", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1,
			}
			if _, err := st.CreateReport(ctx, original); err != nil {
				t.Fatal(err)
			}
			claimed, err := st.ClaimPendingReportBatch(ctx, original.ProjectID, "crashed-owner", now)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim=%+v %v", claimed, err)
			}
			key := claimed[0].DeliveryBatchID
			target := session
			if tc.name == "cross-project" {
				seedProject(t, st, "foreign")
				foreign := sampleRecord("foreign")
				foreign.Mode = domain.SessionModeChat
				target, err = st.CreateSession(ctx, foreign)
				if err != nil {
					t.Fatal(err)
				}
			}
			conv, err := st.CreateConversation(ctx, "claim-conversation", domain.ConversationScopeSession, target.ProjectID, target.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ClaimChatControllerGeneration(ctx, target.ID, "claim-generation"); err != nil {
				t.Fatal(err)
			}
			content, _ := json.Marshal([]ports.ChatContent{{Type: "text", Internal: true, URI: ports.ChatWorkerReportURIPrefix + key, Text: "<ao-worker-reports>\nimmutable report payload\n</ao-worker-reports>"}})
			message := domain.ConversationMessage{ID: "claim-message", Text: "human authored request", Origin: domain.MessageOriginHuman, ClientMessageID: "human-client", DeliveryContentJSON: string(content)}
			switch tc.name {
			case "standalone-automation":
				message.Text = "immutable report payload"
				message.Origin = domain.MessageOriginAutomation
				message.ClientMessageID = key
				message.DeliveryContentJSON = ""
			case "ordinary-user-prefix":
				message.ClientMessageID = key
				message.DeliveryContentJSON = ""
			case "malformed-content":
				message.DeliveryContentJSON = "{invalid"
			case "scalar-content":
				message.DeliveryContentJSON = "[" + `"not a structured internal block"` + "]"
			}
			if tc.name != "never-appended" {
				if created, err := st.AppendUserMessage(ctx, conv.ID, target.ID, "claim-generation", message, "claim-turn", now); err != nil || !created {
					t.Fatalf("append=%v %v", created, err)
				}
			}
			if tc.name == "cancelled" {
				if err := st.CancelQueuedTurnByID(ctx, conv.ID, "claim-turn", now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "uncertain-write" || tc.name == "interrupted-after-write" {
				if err := st.RecordProviderInput(ctx, "claim-turn", "human authored request\n\nimmutable report payload"); err != nil {
					t.Fatal(err)
				}
				if err := st.MarkTurnDispatching(ctx, "claim-turn"); err != nil {
					t.Fatal(err)
				}
				if err := st.SettleTurnByID(ctx, "claim-turn", domain.TurnStateFailed, "DISPATCH_OUTCOME_UNCERTAIN", now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "interrupted-before-write" || tc.name == "interrupted-after-write" {
				if err := st.SettleTurnByID(ctx, "claim-turn", domain.TurnStateInterrupted, "", now); err != nil {
					t.Fatal(err)
				}
			}
			// The process disappears between durable message append and report ACK.
			// Reopen the real store and run the same recovery used by daemon startup.
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = sqlite.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if count, err := st.RequeueClaimedReports(ctx); err != nil || count != 1 {
				t.Fatalf("recovery=%d %v", count, err)
			}
			report, found, err := st.GetReport(ctx, original.ID)
			want := domain.ReportPending
			if tc.accepted {
				want = domain.ReportAcknowledged
			}
			if err != nil || !found || report.DeliveryState != want || report.DeliveryBatchID != key || report.Note != original.Note || report.DeliveryAttempts != 1 {
				t.Fatalf("recovered=%+v %v", report, err)
			}
			if count, err := st.RequeueClaimedReports(ctx); err != nil || count != 0 {
				t.Fatalf("recovery repeated=%d %v", count, err)
			}
			if tc.name != "never-appended" {
				saved, found, err := st.ConversationMessageByClientID(ctx, conv.ID, message.ClientMessageID)
				if err != nil || !found || saved.TurnID != "claim-turn" || saved.Text != message.Text || saved.DeliveryContentJSON != message.DeliveryContentJSON {
					t.Fatalf("recovery rewrote the original: %+v %v", saved, err)
				}
			}
			next, err := st.ClaimPendingReportBatch(ctx, original.ProjectID, "new-owner", now)
			if err != nil {
				t.Fatal(err)
			}
			if tc.accepted && len(next) != 0 || !tc.accepted && len(next) != 1 {
				t.Fatalf("next claim=%+v", next)
			}
		})
	}
}
