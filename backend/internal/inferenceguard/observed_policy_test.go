package inferenceguard

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestObservedHighPolicyStopSurvivesRestartAndAllowanceCannotClearIt(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "authority.db")
	open := func() *Gate {
		g, e := Open(path, func() (Policy, error) { return p, nil }, func() time.Time { return now })
		if e != nil {
			t.Fatal(e)
		}
		return g
	}
	g := open()
	defer func() { _ = g.Close() }()
	r := request(t, g, "native-attempt", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, r.ID, "native-turn"); err != nil {
		t.Fatal(err)
	}
	if err := g.IngestReceipt(ctx, UsageReceipt{SourceKey: "native-actual-high", Session: r.Session, ProviderTurnID: "native-turn", Model: "sol", Effort: "high", ObservedAt: now, InputTokens: 50, OutputTokens: 7, Confidence: "provider_response"}); err != nil {
		t.Fatal(err)
	}
	if err := g.ObserveNativeSelection(ctx, r.Session, "native-turn", "sol", "high"); !errors.Is(err, ErrPolicy) || PreDispatchRefusal(err) {
		t.Fatalf("post-dispatch drift classification %v", err)
	}
	if err := g.Trip(ctx, time.Time{}, "separate quota failure"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := g.ConfirmAllowance(ctx, p.AccountKey, "fake fresh capacity", now, true); !errors.Is(err, ErrPolicy) {
		t.Fatal("allowance cleared native policy violation", err)
	}
	if err := g.Check(ctx); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	_ = g.Close()
	g = open()
	s, err := g.Snapshot(ctx)
	if err != nil || s.Quota != "POLICY_VIOLATION" || !s.PolicyStop || !s.LatchedStop {
		t.Fatalf("%+v %v", s, err)
	}
	foundActual := false
	for _, m := range s.Models {
		if m.Effort == "high" && m.Receipts == 1 && m.Input == 50 {
			foundActual = true
		}
	}
	if !foundActual {
		t.Fatal("actual High usage was hidden or relabeled Medium")
	}
	p.EmergencyStop = true
	if err := g.ReviewPolicyViolation(ctx, "owner", "verified model policy after fixture"); !errors.Is(err, ErrPolicy) {
		t.Fatal("uncertain work allowed policy clear", err)
	}
	if err := g.Complete(ctx, r.Session, "native-turn", "policy_violation"); err != nil {
		t.Fatal(err)
	}
	if err := g.ReviewPolicyViolation(ctx, "owner", "verified model policy after fixture"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := g.ConfirmAllowance(ctx, p.AccountKey, "fake fresh capacity after owner review", now, true); err != nil {
		t.Fatal(err)
	}
	p.EmergencyStop = false
	if err := g.Check(ctx); err != nil {
		t.Fatal(err)
	}
	// Rotating an already reviewed receipt cannot invent a new violation.
	if err := g.ObserveNativeSelection(ctx, r.Session, "native-turn", "sol", "high"); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(ctx); err != nil {
		t.Fatal(err)
	}
}
