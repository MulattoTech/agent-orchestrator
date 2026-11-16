package sessionmanager

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// HealthSnapshot reads child/controller evidence; it never reconciles, connects,
// starts, interrupts or kills a provider. HTTP health and durable session rows
// alone cannot establish worker health. Runtime probes honor existing ownership
// generations and uncertainty, with a bounded observation budget.
func (m *Manager) HealthSnapshot(ctx context.Context) (domain.SessionHealthSnapshot, error) {
	out := domain.SessionHealthSnapshot{ObservedAt: m.clock().UTC(), Status: "UNKNOWN"}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	budget, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := m.store.ListAllSessions(budget)
	if err != nil {
		return out, err
	}
	active := make([]domain.SessionRecord, 0, len(rows))
	for _, row := range rows {
		if !row.IsTerminated && !row.IsTaskPreparation {
			active = append(active, row)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	out.TotalActiveSessions = len(active)
	if len(active) > 256 {
		active = active[:256]
		out.Truncated = true
	}
	out.Sessions = make([]domain.SessionControllerHealth, len(active))
	jobs := make(chan int, len(active))
	for i := range active {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(active)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				out.Sessions[i] = m.observeSessionHealth(budget, active[i])
			}
		}()
	}
	workers.Wait()
	for _, row := range out.Sessions {
		switch row.ControllerState {
		case "stopped":
			out.StoppedSessions++
		case "hibernated":
			out.HibernatedSessions++
		case "provisioning":
			out.ProvisioningSessions++
		}
		if !row.Expected {
			continue
		}
		out.ExpectedControllers++
		switch row.ControllerState {
		case "alive":
			out.AliveControllers++
		case "absent":
			out.MissingControllers++
		default:
			out.UnknownControllers++
		}
		if row.ProcessState == "alive" {
			out.VerifiedRuntimeProcesses++
		}
		if row.ProcessState == "unknown" {
			out.UnknownProcesses++
		}
	}
	switch {
	case out.MissingControllers > 0:
		out.Status = "DEGRADED"
	case out.UnknownControllers > 0 || out.UnknownProcesses > 0 || out.Truncated:
		out.Status = "UNKNOWN"
	case out.ExpectedControllers == 0 && out.ProvisioningSessions > 0:
		out.Status = "PROVISIONING"
	case out.ExpectedControllers == 0:
		out.Status = "IDLE"
	default:
		out.Status = "HEALTHY"
	}
	return out, nil
}

func (m *Manager) observeSessionHealth(ctx context.Context, rec domain.SessionRecord) domain.SessionControllerHealth {
	mode := domain.NormalizeSessionMode(rec.Mode)
	result := domain.SessionControllerHealth{SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: rec.Kind, Mode: mode,
		Generation: rec.Metadata.RuntimeLaunchID, Expected: true, ControllerState: "unknown", ProcessState: "unknown"}
	if mode == domain.SessionModeChat {
		result.Generation = rec.Metadata.ControllerGeneration
	}
	switch {
	case rec.ProvisionState.IsProvisioning():
		result.Expected = false
		result.ControllerState = "provisioning"
		result.ProcessState = "not_expected"
		result.Evidence = "durable_provisioning"
		return result
	case rec.HibernatedAt != nil:
		result.Expected = false
		result.ControllerState = "hibernated"
		result.ProcessState = "not_expected"
		result.Evidence = "durable_hibernation"
		return result
	case rec.RuntimeReleased() || rec.Activity.State == domain.ActivityExited:
		result.Expected = false
		result.ControllerState = "stopped"
		result.ProcessState = "not_expected"
		result.Evidence = "durable_stopped"
		return result
	case ctx.Err() != nil:
		result.Evidence = "observation_budget"
		return result
	case m.SessionMutationInProgress(rec.ID):
		result.Evidence = "session_operation_in_progress"
		return result
	}
	if mode == domain.SessionModeChat {
		if m.chat == nil {
			result.Evidence = "chat_observer_unavailable"
			return result
		}
		if m.chat.HasLiveChatController(rec.ID) {
			result.ControllerState = "alive"
			result.Evidence = "chat_event_controller_attached_process_unverified"
		} else {
			result.ControllerState = "absent"
			result.Evidence = "chat_event_controller_absent"
		}
	} else {
		handle := runtimeHandle(rec.Metadata)
		if m.runtime == nil || handle.ID == "" || rec.Metadata.RuntimeLaunchID == "" {
			result.Evidence = "runtime_identity_missing"
			return result
		}
		probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		probe := m.runtime.ProbeFencedRuntime(probeCtx, ports.FencedRuntimeRef{
			Handle: handle, SessionID: rec.ID, Generation: rec.Metadata.RuntimeLaunchID,
			NativeIdentity: rec.Metadata.AgentSessionID,
		})
		cancel()
		result.Evidence = string(probe.Reason)
		switch probe.Liveness {
		case ports.FencedAlive:
			result.ControllerState = "alive"
			result.ProcessState = "alive"
		case ports.FencedDead:
			result.ControllerState = "absent"
			result.ProcessState = "absent"
		}
	}
	// A correct probe of an old generation cannot be reported as the current
	// controller's health after a concurrent restore or interface handoff.
	current, found, err := m.store.GetSession(ctx, rec.ID)
	if err != nil || !found || current.ControllerOwner() != rec.ControllerOwner() || current.Metadata.RuntimeHandleID != rec.Metadata.RuntimeHandleID || current.Activity.State != rec.Activity.State {
		result.ControllerState = "unknown"
		result.ProcessState = "unknown"
		result.Evidence = "owner_changed_or_unreadable"
	}
	return result
}
