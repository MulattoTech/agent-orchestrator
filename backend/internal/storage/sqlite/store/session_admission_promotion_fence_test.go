package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The PR38 seed-fence migration protects INSERT ON sessions. A speculative
// task-preparation seed is inserted without a client key and then receives
// one through PromoteTaskPreparation's UPDATE. That update must not claim a
// key owned by another Manager's durable queued command.
//
// All fixtures are temporary SQLite; no runtime, provider or network calls.
func TestTaskPreparationPromotionCannotBypassQueuedClientKeyFence(t *testing.T) {
	const key = "synthetic-promotion-global-client-key"
	const queueHash = "queue-v1"
	for _, tt := range []struct {
		name          string
		queuedProject string
		queuedState   string
		promotionHash string
		allowed       bool
	}{
		{"other_project_queued_key_rejected", "green", "queued", "promotion-v2", false},
		{"same_project_queued_key_rejected", "blue", "queued", queueHash, false},
		{"matching_running_dispatch_allowed", "blue", "running", queueHash, true},
		{"running_changed_payload_rejected", "blue", "running", "changed-v2", false},
		{"running_other_project_rejected", "green", "running", queueHash, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			seedProject(t, store, "blue")
			seedProject(t, store, "green")

			prep := sampleRecord("blue")
			prep.IsTaskPreparation = true
			prep.ProvisionState = domain.SessionProvisionProvisioning
			prep.ClientRequestID = ""
			prep.ClientRequestHash = ""
			created, err := store.CreateSession(ctx, prep)
			if err != nil {
				t.Fatalf("insert hidden preparation: %v", err)
			}
			now := time.Now().UTC().Add(-time.Second)
			queued := domain.SessionAdmissionRequest{
				ID:        "synthetic-queued-promotion-key",
				ProjectID: domain.ProjectID(tt.queuedProject),
				Operation: "spawn",
				Payload:   `{"ProjectID":"` + tt.queuedProject + `","ClientRequestID":"` + key + `","ClientRequestHash":"` + queueHash + `"}`,
				CreatedAt: now, UpdatedAt: now, AvailableAt: now,
			}
			if err := store.EnqueueSessionAdmission(ctx, queued); err != nil {
				t.Fatalf("enqueue durable key: %v", err)
			}
			if tt.queuedState == "running" {
				claim, ok, err := store.ClaimSessionAdmission(ctx, "synthetic-owned-boot", now.Add(time.Second))
				if err != nil || !ok || claim.ID != queued.ID {
					t.Fatalf("claim queued key: ok=%t claim=%+v err=%v", ok, claim, err)
				}
			}
			visible := sampleRecord("blue")
			visible.ClientRequestID = key
			visible.ClientRequestHash = tt.promotionHash
			promoted, err := store.PromoteTaskPreparation(ctx, created.ID, visible)

			owner, found, lookupErr := store.GetSessionByClientRequestID(ctx, key)
			if lookupErr != nil {
				t.Fatalf("read durable key: %v", lookupErr)
			}
			stored, exists, getErr := store.GetSession(ctx, created.ID)
			if getErr != nil || !exists {
				t.Fatalf("read hidden/prepared row: exists=%t err=%v", exists, getErr)
			}
			if tt.allowed {
				if err != nil || !promoted || !found || owner.ID != created.ID ||
					stored.IsTaskPreparation || stored.ClientRequestHash != tt.promotionHash {
					t.Fatalf("matching running dispatcher rejected: promoted=%t err=%v found=%t owner=%s stored=%+v",
						promoted, err, found, owner.ID, stored)
				}
			} else {
				if promoted || !errors.Is(err, sqlitestore.ErrAdmissionClientKeyConflict) {
					t.Errorf("incompatible preparation stole queued client key: promoted=%t err=%v", promoted, err)
				}
				if found || stored.ClientRequestID != "" || !stored.IsTaskPreparation {
					t.Errorf("rejected preparation persisted key: found=%t owner=%s storedID=%q hidden=%t",
						found, owner.ID, stored.ClientRequestID, stored.IsTaskPreparation)
				}
			}
			requests, listErr := store.ListSessionAdmission(ctx)
			if listErr != nil || len(requests) != 1 {
				t.Errorf("queue claim lost during promotion: %+v err=%v", requests, listErr)
			}
		})
	}
}
