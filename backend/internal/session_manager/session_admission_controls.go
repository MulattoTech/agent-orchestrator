package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type sessionAdmissionSettingsStore interface {
	GetSessionAdmissionLimits(context.Context) (domain.SessionAdmissionLimits, bool, error)
	SetSessionAdmissionLimits(context.Context, domain.SessionAdmissionLimits, time.Time) error
}

func cloneAdmissionLimits(limits domain.SessionAdmissionLimits) domain.SessionAdmissionLimits {
	if limits.ProjectWorkers != nil {
		projects := make(map[domain.ProjectID]int, len(limits.ProjectWorkers))
		for project, ceiling := range limits.ProjectWorkers {
			projects[project] = ceiling
		}
		limits.ProjectWorkers = projects
	}
	return limits
}

// UpdateAdmissionLimits durably records operator controls before publishing the
// new ceilings under the same lock as admission reservations. Lowering a ceiling
// never kills existing workers or removes work. Already claimed commands can
// finish; the dispatcher checks QueueEnabled before every subsequent claim.
func (m *Manager) UpdateAdmissionLimits(ctx context.Context, limits domain.SessionAdmissionLimits) error {
	limits = cloneAdmissionLimits(limits)
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("invalid admission limits: %w", err)
	}
	store, ok := m.store.(sessionAdmissionSettingsStore)
	if !ok {
		return errors.New("persistent admission controls unavailable")
	}
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SetSessionAdmissionLimits(ctx, limits, m.clock()); err != nil {
		return fmt.Errorf("persist admission controls: %w", err)
	}
	m.admission.limits = limits
	return nil
}

// LoadAdmissionLimits runs during pre-listener startup safety reconciliation.
// Durable controls override bootstrap defaults. Corrupt settings stop startup;
// they must never silently open an unrestricted admission path.
func (m *Manager) LoadAdmissionLimits(ctx context.Context) error {
	store, ok := m.store.(sessionAdmissionSettingsStore)
	if !ok {
		return nil
	}
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	limits, found, err := store.GetSessionAdmissionLimits(ctx)
	if err != nil {
		return fmt.Errorf("load admission controls: %w", err)
	}
	if found {
		if err := limits.Validate(); err != nil {
			return err
		}
		m.admission.limits = cloneAdmissionLimits(limits)
	}
	return nil
}
