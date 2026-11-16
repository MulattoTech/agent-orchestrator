package report

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCoordinatorValidatesDurableAcceptanceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, id, turn string
		ack            bool
	}{
		{"exact", "report-batch:report-1", "turn-1", true},
		{"other-batch", "report-batch:other", "turn-1", false},
		{"missing-turn", "report-batch:report-1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
			st := newCoordinatorStore(now)
			st.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
			rec := reportAt("report-1", "worker", now, now, "report")
			rec.DeliveryBatchID = "report-batch:report-1"
			st.add(rec)
			cause := errors.New("synthetic uncertain provider write")
			delivery := &coordinatorDelivery{submitErr: fmt.Errorf("relay: %w", &domain.ReportDeliveryAcceptedError{DeliveryID: tc.id, TurnID: tc.turn, Cause: cause})}
			c := NewCoordinator(CoordinatorDeps{Store: st, Delivery: delivery, Now: func() time.Time { return now }})
			if _, err := c.RunDue(context.Background()); !errors.Is(err, cause) {
				t.Fatalf("lost underlying error: %v", err)
			}
			want := domain.ReportPending
			if tc.ack {
				want = domain.ReportAcknowledged
			}
			if st.reports[0].DeliveryState != want {
				t.Fatalf("report=%+v", st.reports[0])
			}
		})
	}
}
