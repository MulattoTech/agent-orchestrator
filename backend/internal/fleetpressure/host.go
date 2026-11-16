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
	mu       sync.Mutex
	previous procmem.System
	read     func() (procmem.System, error)
	handles  func(context.Context) (int, error)
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
	if n, e := s.handles(ctx); e == nil {
		m.ProcessHandles = &n
	}
	return m, nil
}
