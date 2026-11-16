package fleetpressure

import (
	"math"
	"testing"
	"time"
)

func measurement(at time.Time) Measurement {
	memory := uint64(64 * GiB)
	cpu := float64(20)
	handles := 100
	trees := 40
	accepted := int64(0)
	return Measurement{At: at, AvailableBytes: &memory, CPUPercent: &cpu, ProcessHandles: &handles, Worktrees: &trees, ActiveWorkers: 2, QueuedTasks: 40, QuotaState: "available", AcceptedResults: &accepted}
}

func TestPressureNeverExceedsOwnerCapAndRequiresUsefulProgress(t *testing.T) {
	now := time.Now()
	c := New(DefaultLimits(4))
	m := measurement(now)
	for i := 0; i < 20; i++ {
		m.At = now.Add(time.Duration(i) * time.Second)
		d := c.Evaluate(m.At, m)
		if d.WorkerCeiling != 2 {
			t.Fatalf("expanded without accepted work: %+v", d)
		}
	}
	for i := 20; i < 60; i++ {
		m.At = now.Add(time.Duration(i) * time.Second)
		*m.AcceptedResults = int64(i)
		d := c.Evaluate(m.At, m)
		if d.WorkerCeiling > 4 {
			t.Fatalf("overshot owner cap: %+v", d)
		}
	}
	if c.Snapshot().WorkerCeiling != 4 {
		t.Fatal("did not expand after measured progress")
	}
}
func TestPressureUnknownQuotaAndSamplesNeverExpand(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"quota", "handles", "worktrees", "acceptance", "cpu"} {
		t.Run(kind, func(t *testing.T) {
			c := New(DefaultLimits(40))
			m := measurement(now)
			switch kind {
			case "quota":
				m.QuotaState = "unknown"
			case "handles":
				m.ProcessHandles = nil
			case "worktrees":
				m.Worktrees = nil
			case "acceptance":
				m.AcceptedResults = nil
			case "cpu":
				m.CPUPercent = nil
			}
			for i := 0; i < 20; i++ {
				m.At = now.Add(time.Duration(i) * time.Second)
				d := c.Evaluate(m.At, m)
				if d.WorkerCeiling > 2 || d.Reason != "expansion_metrics_unknown" {
					t.Fatalf("unsafe expansion %+v", d)
				}
			}
		})
	}
}
func TestPressureStopsNewAdmissionWithoutMutatingActiveWorkers(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"memory", "handles", "worktrees", "quota", "stale", "future", "missing_memory", "invalid_cpu"} {
		t.Run(kind, func(t *testing.T) {
			c := New(DefaultLimits(40))
			m := measurement(now)
			m.ActiveWorkers = 20
			switch kind {
			case "memory":
				*m.AvailableBytes = 7 * GiB
			case "handles":
				*m.ProcessHandles = 100001
			case "worktrees":
				*m.Worktrees = 500
			case "quota":
				m.QuotaState = "exhausted"
			case "stale":
				m.At = now.Add(-2 * time.Minute)
			case "future":
				m.At = now.Add(time.Hour)
			case "missing_memory":
				m.AvailableBytes = nil
			case "invalid_cpu":
				*m.CPUPercent = math.NaN()
			}
			d := c.Evaluate(now, m)
			if d.WorkerCeiling != 0 {
				t.Fatalf("should stop admissions %+v", d)
			}
			if m.ActiveWorkers != 20 {
				t.Fatal("mutated active workers")
			}
		})
	}
}
func TestPressureRecoveryAndReplay(t *testing.T) {
	now := time.Now()
	c := New(DefaultLimits(20))
	m := measurement(now)
	*m.AvailableBytes = 7 * GiB
	if d := c.Evaluate(now, m); d.WorkerCeiling != 0 {
		t.Fatal(d)
	}
	m.At = now.Add(time.Second)
	*m.AvailableBytes = 64 * GiB
	if d := c.Evaluate(m.At, m); d.WorkerCeiling != 2 {
		t.Fatal(d)
	}
	if d := c.Evaluate(m.At, m); d.WorkerCeiling != 0 || d.Reason != "stale_or_replayed_measurement" {
		t.Fatal(d)
	}
}
func TestPressureMemoryAllocationAndCPUBackoff(t *testing.T) {
	now := time.Now()
	c := New(DefaultLimits(40))
	m := measurement(now)
	*m.AvailableBytes = 9 * GiB
	m.ActiveWorkers = 1
	if d := c.Evaluate(now, m); d.WorkerCeiling != 1 {
		t.Fatal(d)
	}
	m.At = now.Add(time.Second)
	*m.CPUPercent = 95
	m.ActiveWorkers = 2
	if d := c.Evaluate(m.At, m); d.WorkerCeiling != 1 || d.Reason != "cpu_pressure" {
		t.Fatal(d)
	}
}
func TestPressureDoesNotReuseHistoricalAcceptanceOrMutablePointers(t *testing.T) {
	now := time.Now()
	c := New(DefaultLimits(40))
	m := measurement(now)
	*m.AcceptedResults = 100
	for i := 0; i < 20; i++ {
		m.At = now.Add(time.Duration(i) * time.Second)
		if d := c.Evaluate(m.At, m); d.WorkerCeiling != 2 {
			t.Fatal(d)
		}
	}
	*m.AvailableBytes = 0
	if *c.Snapshot().Measurement.AvailableBytes == 0 {
		t.Fatal("caller mutated snapshot")
	}
	d := c.Snapshot()
	*d.Measurement.AvailableBytes = 0
	if *c.Snapshot().Measurement.AvailableBytes == 0 {
		t.Fatal("snapshot aliases internal state")
	}
}
