//go:build windows

package fleetpressure

import (
	"context"
	"testing"
	"time"
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
	if m.ProcessHandles == nil || *m.ProcessHandles <= 0 {
		t.Fatalf("no native process handles %+v", m)
	}
	if m.CPUPercent != nil {
		t.Fatal("first sample must not invent CPU rate")
	}
}
