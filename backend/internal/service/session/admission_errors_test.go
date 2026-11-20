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

func TestAdmissionTerminalErrorsPreserveDurableState(t *testing.T) {
	for _, state := range []string{"cancelled", "blocked", "completed"} {
		t.Run(state, func(t *testing.T) {
			mapped := toSpawnAPIError(&sessionmanager.SessionAdmissionStateError{RequestID: "admit-terminal-id", State: state})
			var response *apierr.Error
			if !errors.As(mapped, &response) || response.Kind != apierr.KindConflict ||
				response.Code != "SESSION_ADMISSION_NOT_RUNNABLE" ||
				response.Details["admissionRequestId"] != "admit-terminal-id" || response.Details["state"] != state {
				t.Fatalf("terminal command lost its durable identity/state: %+v", mapped)
			}
		})
	}
}
