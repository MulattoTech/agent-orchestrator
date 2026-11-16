package inferenceguard

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestModelUsageDoesNotReportTransportOrRestartUncertaintyAsFailure(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "guard.db")
	open := func() *Gate {
		g, err := Open(path, func() (Policy, error) { return p, nil }, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = g.Close() })
		return g
	}
	g := open()
	failed := request(t, g, "confirmed-failure", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, failed, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, failed.ID, "failed-native-turn"); err != nil {
		t.Fatal(err)
	}
	if err := g.Complete(ctx, failed.Session, "failed-native-turn", "provider_error"); err != nil {
		t.Fatal(err)
	}
	transport := request(t, g, "transport-unknown", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, transport, func() error { return errors.New("synthetic lost transport acknowledgement") }); !errors.Is(err, ErrUncertain) {
		t.Fatalf("transport outcome was not uncertain: %v", err)
	}
	active := request(t, g, "restart-unknown", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, active, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, active.ID, "active-native-turn"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot := func(wantActive, wantUncertain, wantCompleted, wantFailed int) {
		t.Helper()
		s, err := g.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.Active != wantActive || s.Uncertain != wantUncertain || s.Completed != wantCompleted || len(s.Models) != 1 || s.Models[0].Turns != 3 || s.Models[0].Failed != wantFailed {
			t.Fatalf("model usage confuses uncertain transport/restart outcomes with confirmed failures: %+v", s)
		}
		rows, err := g.SessionUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var failed, uncertain int
		for _, row := range rows {
			failed += row.Failed
			uncertain += row.Uncertain
		}
		if failed != wantFailed || uncertain != wantUncertain {
			t.Fatalf("model and session totals disagree: failed=%d uncertain=%d rows=%+v", failed, uncertain, rows)
		}
	}
	assertSnapshot(1, 1, 1, 1)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g = open()
	assertSnapshot(0, 2, 1, 1)
	// Only explicit completed outcomes change the failed total.
	if err := g.Complete(ctx, active.Session, "active-native-turn", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.SettleRequest(ctx, transport.ID, "provider_error"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(0, 0, 3, 2)
}
