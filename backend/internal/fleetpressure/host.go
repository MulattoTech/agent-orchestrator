package fleetpressure

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
)

// HostSampler reads local operating-system counters only. No account, HTTP,
// shell-agent, or inference call is part of resource observation.
type HostSampler struct {
	mu          sync.Mutex
	previous    procmem.System
	read        func() (procmem.System, error)
	handles     func(context.Context) (int, error)
	lastHandles HandleObservation
}

// HandleObservation retains a measured lower bound without claiming a complete
// managed-fleet inventory. Detached or recovered native hosts may not descend
// from the current daemon. Unknown coverage blocks automatic expansion beyond
// the initial worker ceiling; the owner's worker limit remains independent.
type HandleObservation struct {
	At                time.Time `json:"at"`
	Count             *int      `json:"count"`
	Scope             string    `json:"scope"`
	FleetComplete     bool      `json:"fleetComplete"`
	CoverageReason    string    `json:"coverageReason"`
	UnavailableReason string    `json:"unavailableReason,omitempty"`
}

// PressureLowerBound returns a fresh partial count only when that lower bound
// already proves excessive handles. A low partial count never proves health.
func (h HandleObservation) PressureLowerBound(at time.Time, maximum int) *int {
	if h.At.Equal(at) && h.Count != nil && maximum > 0 && *h.Count >= maximum {
		return copied(h.Count)
	}
	return nil
}

func (s *HostSampler) HandleObservation() HandleObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.lastHandles
	h.Count = copied(h.Count)
	return h
}

func NewHostSampler() *HostSampler {
	return &HostSampler{read: procmem.ReadSystem, handles: readProcessTreeHandles}
}

func (s *HostSampler) Sample(ctx context.Context, at time.Time) (Measurement, error) {
	if err := ctx.Err(); err != nil {
		return Measurement{At: at}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Measurement{At: at, QuotaState: "unknown"}
	s.lastHandles = HandleObservation{At: at, Scope: "current_daemon_descendants_only",
		FleetComplete: false, CoverageReason: "detached_or_recovered_native_hosts_not_covered",
		UnavailableReason: "not_sampled"}
	sys, err := s.read()
	if err != nil {
		return m, err
	}
	m.AvailableBytes = copied(&sys.AvailableBytes)
	if s.previous.CPUTotalTicks > 0 && sys.CPUTotalTicks > s.previous.CPUTotalTicks && sys.CPUBusyTicks >= s.previous.CPUBusyTicks {
		delta := sys.CPUTotalTicks - s.previous.CPUTotalTicks
		busy := sys.CPUBusyTicks - s.previous.CPUBusyTicks
		if busy <= delta {
			cpu := float64(busy) / float64(delta) * 100
			m.CPUPercent = &cpu
		}
	}
	s.previous = sys
	if n, e := s.handles(ctx); e == nil && n >= 0 {
		s.lastHandles.Count = &n
		s.lastHandles.UnavailableReason = ""
	} else {
		s.lastHandles.UnavailableReason = "native_counter_unavailable"
	}
	// ProcessHandles intentionally stays nil: a successful own-tree read is
	// not a complete fleet count and cannot authorize automatic expansion.
	return m, nil
}
