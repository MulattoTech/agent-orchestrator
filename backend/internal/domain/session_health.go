package domain

import "time"

// SessionControllerHealth separates durable expectations, attached controllers
// and exact-generation process evidence. Unknown is never treated as absent.
type SessionControllerHealth struct {
	SessionID       SessionID   `json:"sessionId"`
	ProjectID       ProjectID   `json:"projectId"`
	Kind            SessionKind `json:"kind"`
	Mode            SessionMode `json:"mode"`
	Generation      string      `json:"generation,omitempty"`
	Expected        bool        `json:"expected"`
	ControllerState string      `json:"controllerState"`
	ProcessState    string      `json:"processState"`
	Evidence        string      `json:"evidence"`
}

type SessionHealthSnapshot struct {
	ObservedAt           time.Time `json:"observedAt"`
	Status               string    `json:"status"`
	TotalActiveSessions  int       `json:"totalActiveSessions"`
	ExpectedControllers  int       `json:"expectedControllers"`
	AliveControllers     int       `json:"aliveControllers"`
	MissingControllers   int       `json:"missingControllers"`
	UnknownControllers   int       `json:"unknownControllers"`
	UnknownProcesses     int       `json:"unknownProcesses"`
	StoppedSessions      int       `json:"stoppedSessions"`
	HibernatedSessions   int       `json:"hibernatedSessions"`
	ProvisioningSessions int       `json:"provisioningSessions"`
	// This is a verified lower bound, not a guessed count of tool descendants.
	VerifiedRuntimeProcesses int                       `json:"verifiedRuntimeProcesses"`
	RunningToolProcesses     *int                      `json:"runningToolProcesses"`
	Truncated                bool                      `json:"truncated"`
	Sessions                 []SessionControllerHealth `json:"sessions"`
}
