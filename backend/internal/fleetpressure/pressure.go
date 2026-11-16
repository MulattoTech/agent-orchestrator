// Package fleetpressure bounds worker admission using measured host pressure.
// It never starts providers, terminates workers, or raises the owner's ceiling.
package fleetpressure

import (
	"math"
	"sync"
	"time"
)

const GiB uint64 = 1 << 30

type Limits struct {
	MaxWorkers           int
	InitialWorkers       int
	ReserveBytes         uint64
	EstimatedWorkerBytes uint64
	MaxCPUPercent        float64
	MaxProcessHandles    int
	MaxWorktrees         int
	HealthySamples       int
	MaxSampleAge         time.Duration
}

func DefaultLimits(maxWorkers int) Limits {
	return Limits{MaxWorkers: maxWorkers, InitialWorkers: min(2, maxWorkers), ReserveBytes: 8 * GiB,
		EstimatedWorkerBytes: 2 * GiB, MaxCPUPercent: 85, MaxProcessHandles: 100000,
		MaxWorktrees: 500, HealthySamples: 3, MaxSampleAge: 90 * time.Second}
}

// Measurement uses pointers for unavailable facts. Unknown is never zero.
type Measurement struct {
	At              time.Time `json:"at"`
	AvailableBytes  *uint64   `json:"availableBytes"`
	CPUPercent      *float64  `json:"cpuPercent"`
	ProcessHandles  *int      `json:"processHandles"`
	Worktrees       *int      `json:"worktrees"`
	ActiveWorkers   int       `json:"activeWorkers"`
	QueuedTasks     int       `json:"queuedTasks"`
	QuotaState      string    `json:"quotaState"`
	AcceptedResults *int64    `json:"acceptedResults"`
}

type Decision struct {
	WorkerCeiling        int         `json:"workerCeiling"`
	Reason               string      `json:"reason"`
	HealthySamples       int         `json:"healthySamples"`
	Measurement          Measurement `json:"measurement"`
	EstimatedWorkerBytes uint64      `json:"estimatedWorkerBytes"`
}

type Controller struct {
	mu             sync.Mutex
	limits         Limits
	ceiling        int
	healthy        int
	lastAt         time.Time
	lastAccepted   int64
	acceptanceSeen bool
	last           Decision
}

func New(l Limits) *Controller {
	if l.MaxWorkers < 0 {
		l.MaxWorkers = 0
	}
	if l.InitialWorkers < 0 {
		l.InitialWorkers = 0
	}
	l.InitialWorkers = min(l.InitialWorkers, l.MaxWorkers)
	if l.HealthySamples < 1 {
		l.HealthySamples = 3
	}
	if l.EstimatedWorkerBytes == 0 {
		l.EstimatedWorkerBytes = 2 * GiB
	}
	if l.MaxSampleAge <= 0 {
		l.MaxSampleAge = 90 * time.Second
	}
	return &Controller{limits: l, ceiling: l.InitialWorkers}
}

func copied[T any](v *T) *T {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func clone(m Measurement) Measurement {
	m.AvailableBytes = copied(m.AvailableBytes)
	m.CPUPercent = copied(m.CPUPercent)
	m.ProcessHandles = copied(m.ProcessHandles)
	m.Worktrees = copied(m.Worktrees)
	m.AcceptedResults = copied(m.AcceptedResults)
	return m
}

// Evaluate reduces admission immediately under pressure and increases it by
// at most one worker after fresh healthy samples and a newly accepted result.
// Inference is governed by a separate provider/account authority.
func (c *Controller) Evaluate(now time.Time, m Measurement) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	m = clone(m)
	l := c.limits
	decide := func(n int, reason string) Decision {
		c.ceiling = max(0, min(l.MaxWorkers, n))
		c.last = Decision{c.ceiling, reason, c.healthy, m, l.EstimatedWorkerBytes}
		result := c.last
		result.Measurement = clone(result.Measurement)
		return result
	}
	if m.At.IsZero() || m.At.After(now.Add(time.Second)) || now.Sub(m.At) > l.MaxSampleAge || (!c.lastAt.IsZero() && !m.At.After(c.lastAt)) {
		c.healthy = 0
		return decide(0, "stale_or_replayed_measurement")
	}
	c.lastAt = m.At
	if m.AvailableBytes == nil {
		c.healthy = 0
		return decide(0, "memory_unknown")
	}
	if *m.AvailableBytes <= l.ReserveBytes {
		c.healthy = 0
		return decide(0, "host_memory_reserve")
	}
	availableSlots := int(min(uint64(l.MaxWorkers), (*m.AvailableBytes-l.ReserveBytes)/l.EstimatedWorkerBytes)) + max(0, m.ActiveWorkers)
	upper := min(l.MaxWorkers, availableSlots)
	if m.CPUPercent != nil && (math.IsNaN(*m.CPUPercent) || math.IsInf(*m.CPUPercent, 0) || *m.CPUPercent < 0 || *m.CPUPercent > 100) {
		c.healthy = 0
		return decide(0, "invalid_cpu_measurement")
	}
	if m.CPUPercent != nil && *m.CPUPercent >= l.MaxCPUPercent {
		c.healthy = 0
		return decide(min(upper, max(0, m.ActiveWorkers-1)), "cpu_pressure")
	}
	if m.ProcessHandles != nil && (*m.ProcessHandles < 0 || (l.MaxProcessHandles > 0 && *m.ProcessHandles >= l.MaxProcessHandles)) {
		c.healthy = 0
		return decide(0, "process_handle_pressure")
	}
	if m.Worktrees != nil && (*m.Worktrees < 0 || (l.MaxWorktrees > 0 && *m.Worktrees >= l.MaxWorktrees)) {
		c.healthy = 0
		return decide(0, "worktree_pressure")
	}
	if m.QuotaState == "exhausted" || m.QuotaState == "emergency_stop" || m.QuotaState == "hard_stop" {
		c.healthy = 0
		return decide(0, "quota_blocks_new_admission")
	}
	// Unknown quota/throughput/handles never authorizes automatic expansion.
	if m.QuotaState != "available" || m.AcceptedResults == nil || m.CPUPercent == nil || m.ProcessHandles == nil || m.Worktrees == nil {
		c.healthy = 0
		return decide(min(upper, l.InitialWorkers), "expansion_metrics_unknown")
	}
	if !c.acceptanceSeen {
		c.lastAccepted = *m.AcceptedResults
		c.acceptanceSeen = true
	}
	if *m.AcceptedResults < c.lastAccepted {
		c.healthy = 0
		c.lastAccepted = *m.AcceptedResults
		return decide(min(upper, l.InitialWorkers), "acceptance_counter_reset")
	}
	c.healthy++
	current := min(c.ceiling, upper)
	// After a pressure pause, a bounded initial allocation may recover.
	if current < l.InitialWorkers {
		current = min(upper, l.InitialWorkers)
	}
	if m.QueuedTasks > 0 && c.healthy >= l.HealthySamples && *m.AcceptedResults > c.lastAccepted {
		c.lastAccepted = *m.AcceptedResults
		c.healthy = 0
		return decide(min(upper, current+1), "healthy_progress_step")
	}
	return decide(current, "hold_for_measured_progress")
}

func (c *Controller) Snapshot() Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.last
	d.Measurement = clone(d.Measurement)
	return d
}
