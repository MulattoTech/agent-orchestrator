package fleetpressure

import (
	"context"
	"errors"
	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
	"testing"
	"time"
)

func TestHostSamplerUsesDeltasAndMarksUnavailableHandles(t *testing.T) {
	n := 0
	s := &HostSampler{read: func() (procmem.System, error) {
		n++
		return procmem.System{AvailableBytes: 16 * GiB, CPUTotalTicks: uint64(n * 100), CPUBusyTicks: uint64(n * 25)}, nil
	}, handles: func(context.Context) (int, error) { return 0, errors.New("unavailable") }}
	a, e := s.Sample(context.Background(), time.Now())
	if e != nil || a.CPUPercent != nil || a.ProcessHandles != nil {
		t.Fatalf("%+v %v", a, e)
	}
	b, e := s.Sample(context.Background(), time.Now())
	if e != nil || b.CPUPercent == nil || *b.CPUPercent != 25 || b.ProcessHandles != nil {
		t.Fatalf("%+v %v", b, e)
	}
}
func TestHostSamplerCanceledDoesNotObserve(t *testing.T) {
	s := &HostSampler{read: func() (procmem.System, error) { t.Fatal("sampled after cancel"); return procmem.System{}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Sample(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
