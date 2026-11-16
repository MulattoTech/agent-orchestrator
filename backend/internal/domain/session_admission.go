package domain

import (
	"fmt"
	"strings"
	"time"
)

// SessionAdmissionLimits separate registered team slots from active worker
// occupancy. Model inference has its own provider/account authority.
type SessionAdmissionLimits struct {
	RegisteredSlots       int               `json:"registeredSlots"`
	MaxActiveWorkers      int               `json:"maxActiveWorkers"`
	DefaultProjectWorkers int               `json:"defaultProjectWorkers"`
	ProjectWorkers        map[ProjectID]int `json:"projectWorkers,omitempty"`
	QueueEnabled          bool              `json:"queueEnabled"`
}

// SessionAdmissionRequest is an immutable command plus durable dispatch facts.
// Payload remains private; public status views should omit it.
type SessionAdmissionRequest struct {
	ID          string    `json:"id"`
	ProjectID   ProjectID `json:"projectId"`
	Operation   string    `json:"operation"`
	SessionID   SessionID `json:"sessionId,omitempty"`
	Payload     string    `json:"-"`
	State       string    `json:"state"`
	Generation  int64     `json:"generation"`
	Owner       string    `json:"-"`
	Attempts    int       `json:"attempts"`
	FailureCode string    `json:"failureCode,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	AvailableAt time.Time `json:"availableAt"`
}

type SessionAdmissionSnapshot struct {
	Limits                SessionAdmissionLimits `json:"limits"`
	ResourceWorkerCeiling int                    `json:"resourceWorkerCeiling"`
	ActiveWorkerSessions  int                    `json:"activeWorkerSessions"`
	ProvisioningWorkers   int                    `json:"provisioningWorkers"`
	ReservedWorkers       int                    `json:"reservedWorkers"`
	LiveOrchestrators     int                    `json:"liveOrchestrators"`
	QueuedRequests        int                    `json:"queuedRequests"`
	BlockedRequests       int                    `json:"blockedRequests"`
	ProjectWorkers        map[ProjectID]int      `json:"projectWorkers"`
	// Runtime/child liveness is observed separately, never inferred from a row.
	RunningToolProcesses *int `json:"runningToolProcesses"`
}

// Validate rejects ambiguous or unsafe control values. A resource ceiling of
// zero pauses expansion separately; registered slots describe a fleet preset.
func (l SessionAdmissionLimits) Validate() error {
	valid := false
	for _, preset := range []int{2, 4, 8, 16, 20, 30, 40} {
		valid = valid || l.RegisteredSlots == preset
	}
	if !valid {
		return fmt.Errorf("registeredSlots must be 2, 4, 8, 16, 20, 30 or 40")
	}
	if l.MaxActiveWorkers < 1 || l.MaxActiveWorkers > l.RegisteredSlots {
		return fmt.Errorf("maxActiveWorkers must be in 1..%d", l.RegisteredSlots)
	}
	if l.DefaultProjectWorkers < 1 || l.DefaultProjectWorkers > l.RegisteredSlots {
		return fmt.Errorf("defaultProjectWorkers must be in 1..%d", l.RegisteredSlots)
	}
	for id, n := range l.ProjectWorkers {
		if strings.TrimSpace(string(id)) == "" || n < 1 || n > l.RegisteredSlots {
			return fmt.Errorf("each project worker limit must be in 1..%d", l.RegisteredSlots)
		}
	}
	return nil
}
