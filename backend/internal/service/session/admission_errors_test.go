package session

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

func TestAdmissionErrorsPreserveDurableQueueIdentity(t *testing.T) {
	mapped := toSpawnAPIError(&sessionmanager.SessionQueuedError{RequestID: "admit-stable-id"})
	var response *apierr.Error
	if !errors.As(mapped, &response) || response.Kind != apierr.KindConflict ||
		response.Code != "SESSION_CAPACITY_QUEUED" || response.Details["admissionRequestId"] != "admit-stable-id" {
		t.Fatalf("queued command was hidden behind an opaque failure: %+v", mapped)
	}
	for _, input := range []error{sessionmanager.ErrPRCompletionPending, sessionmanager.ErrWorkerCapacity, sessionmanager.ErrWorkspaceOwned, sessionmanager.ErrOrchestratorOccupied} {
		if !errors.As(toAPIError(input), &response) || response.Kind != apierr.KindConflict {
			t.Fatalf("admission refusal mapped to server failure: %v", input)
		}
	}
}
