package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSendSemanticChatMarksOnlyInternalReportsAndPreservesAcceptanceError(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, _, _ := newChatManager(launcher)
	ctx := context.Background()
	rec, _, _, err := mgr.Spawn(ctx, ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, RequestedMode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key    string
		report bool
	}{
		{"report-batch:valid_1", true}, {"ordinary-relay", false}, {"report-batch:invalid id", false}, {"", false},
	} {
		if err := mgr.SendSemantic(ctx, rec.ID, "durable report", tc.key); err != nil {
			t.Fatal(err)
		}
		last := len(launcher.relayOptions) - 1
		if launcher.relayOptions[last].ReportDelivery != tc.report || launcher.relayIDs[last] != tc.key {
			t.Fatalf("semantic key %q lost options: %+v", tc.key, launcher.relayOptions[last])
		}
	}
	receipt := &domain.ReportDeliveryAcceptedError{DeliveryID: "report-batch:valid_1", TurnID: "held-turn", Cause: inferenceguard.ErrUncertain}
	launcher.turnErr = receipt
	err = mgr.SendSemantic(ctx, rec.ID, "durable report", receipt.DeliveryID)
	var accepted *domain.ReportDeliveryAcceptedError
	if !errors.Is(err, inferenceguard.ErrUncertain) || !errors.As(err, &accepted) || accepted.TurnID != "held-turn" {
		t.Fatalf("manager concealed delivery uncertainty: %v", err)
	}
}
