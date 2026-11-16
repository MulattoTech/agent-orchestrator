package sessionmanager

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

// Every terminal surface is outside the governed provider request boundary.
// Reject it before probes, source teardown or workspace mutations, and again at
// write admission so an existing terminal cannot evade a strict build.
func checkSessionLaunchPolicy(harness domain.AgentHarness, mode domain.SessionMode) error {
	return inferenceguard.CheckLaunch(string(harness), string(domain.NormalizeSessionMode(mode)))
}

func (m *Manager) sessionInputPolicyAllows(id domain.SessionID) bool {
	if !inferenceguard.Enabled() {
		return true
	}
	if m.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec, found, err := m.store.GetSession(ctx, id)
	return err == nil && found && !rec.IsTerminated && checkSessionLaunchPolicy(rec.Harness, rec.Mode) == nil
}
