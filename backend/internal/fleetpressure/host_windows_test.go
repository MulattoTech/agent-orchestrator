//go:build windows

package fleetpressure

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
)

func TestWindowsNativeHostSampleIsReadOnly(t *testing.T) {
	s := NewHostSampler()
	m, err := s.Sample(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.AvailableBytes == nil || *m.AvailableBytes == 0 {
		t.Fatalf("no native memory observation %+v", m)
	}
	h := s.HandleObservation()
	if h.Count == nil || *h.Count <= 0 || h.Scope != "current_daemon_descendants_only" || h.FleetComplete {
		t.Fatalf("native lower-bound provenance unavailable or overstated: %+v", h)
	}
	if m.ProcessHandles != nil {
		t.Fatal("partial native count was presented as a complete fleet count")
	}
	if m.CPUPercent != nil {
		t.Fatal("first sample must not invent CPU rate")
	}
}

func TestPartialWindowsHandleCoverageNeverExpandsAndStillStopsUnderPressure(t *testing.T) {
	n, handles := 0, 100
	s := &HostSampler{
		read: func() (procmem.System, error) {
			n++
			return procmem.System{AvailableBytes: 64 * GiB, CPUTotalTicks: uint64(n * 100), CPUBusyTicks: uint64(n * 20)}, nil
		},
		handles: func(context.Context) (int, error) { return handles, nil },
	}
	limits := DefaultLimits(40)
	c := New(limits)
	base := time.Now()
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		m, err := s.Sample(context.Background(), at)
		if err != nil {
			t.Fatal(err)
		}
		if m.ProcessHandles != nil {
			t.Fatal("partial handles claimed complete fleet coverage")
		}
		h := s.HandleObservation()
		m.ProcessHandles = h.PressureLowerBound(at, limits.MaxProcessHandles)
		trees, accepted := 40, int64(i)
		m.Worktrees, m.AcceptedResults = &trees, &accepted
		m.ActiveWorkers, m.QueuedTasks, m.QuotaState = 2, 40, "available"
		d := c.Evaluate(at, m)
		if d.WorkerCeiling != limits.InitialWorkers || d.Reason != "expansion_metrics_unknown" || h.FleetComplete {
			t.Fatalf("partial low handle count authorized expansion: decision=%+v observation=%+v", d, h)
		}
	}
	handles = limits.MaxProcessHandles + 1
	at := base.Add(21 * time.Second)
	m, err := s.Sample(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	h := s.HandleObservation()
	m.ProcessHandles = h.PressureLowerBound(at, limits.MaxProcessHandles)
	d := c.Evaluate(at, m)
	if d.WorkerCeiling != 0 || d.Reason != "process_handle_pressure" {
		t.Fatalf("known excessive lower bound failed to stop admission: %+v", d)
	}
	if h.PressureLowerBound(at.Add(time.Second), limits.MaxProcessHandles) != nil {
		t.Fatal("stale lower bound was reused as a fresh observation")
	}
}
