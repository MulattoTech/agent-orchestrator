package inferenceguard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnerControlsPersistWithoutClearingQuotaOrBudget(t *testing.T) {
	dir := t.TempDir()
	p := testPolicy()
	p.AuthorityPath = filepath.Join(dir, "authority.db")
	path := filepath.Join(dir, "policy.json")
	data, _ := json.Marshal(p)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PolicyEnv, path)
	g, err := Open(p.AuthorityPath, ReadPolicy, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	yes := true
	four := 4
	two := 2
	if err = g.UpdateControls(context.Background(), Controls{Pause: &yes, MaximumInference: &four, MaximumPerProject: &two, MaximumReviewers: &two}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPolicy()
	if err != nil || !got.Pause || got.MaximumPerProject != 2 || got.AuthorizationEpoch != p.AuthorizationEpoch {
		t.Fatal(got, err)
	}
	if err = g.Trip(context.Background(), time.Time{}, "fake reset with timezone unknown"); err != nil {
		t.Fatal(err)
	}
	no := false
	if err = g.UpdateControls(context.Background(), Controls{Pause: &no, EmergencyStop: &no}); err != nil {
		t.Fatal(err)
	}
	if err = g.Check(context.Background()); !errors.Is(err, ErrQuota) {
		t.Fatalf("controls cleared quota: %v", err)
	}
	invalid := 41
	if err = g.UpdateControls(context.Background(), Controls{MaximumInference: &invalid}); err == nil {
		t.Fatal("unbounded cap accepted")
	}
	reread, _ := ReadPolicy()
	if reread.MaximumInference != 4 {
		t.Fatal("invalid edit persisted")
	}
}
func TestObservedReceiptRequiresActualAccountDispatchBinding(t *testing.T) {
	p := testPolicy()
	p.NativeAccountSHA256 = strings.Repeat("a", 64)
	now := time.Now()
	g := openFake(t, &p, &now)
	r := UsageReceipt{SourceKey: "codex:response:fixture", ProviderTurnID: "native-turn", Session: "session", Model: "astra", Effort: "medium", ObservedAt: now, InputTokens: 10, OutputTokens: 2, Confidence: "provider_response"}
	if err := g.IngestBoundReceipt(context.Background(), r); !errors.Is(err, ErrUnboundReceipt) {
		t.Fatal("historical account guessed", err)
	}
	if err := g.VerifyAccount(context.Background(), p.NativeAccountSHA256); err != nil {
		t.Fatal(err)
	}
	req := request(t, g, "attempt", "project", "implementation", "astra", "medium")
	req.Session = "session"
	req.NativeThreadID = "00000000-0000-4000-8000-000000000001"
	if err := g.RegisterNativeThread(context.Background(), req.Session, req.NativeThreadID); err != nil {
		t.Fatal(err)
	}
	r.NativeThreadID = req.NativeThreadID
	if err := g.Dispatch(context.Background(), req, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(context.Background(), "attempt", "native-turn"); err != nil {
		t.Fatal(err)
	}
	if err := g.IngestBoundReceipt(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	missingThread := r
	missingThread.NativeThreadID = ""
	if err := g.IngestBoundReceipt(context.Background(), missingThread); !errors.Is(err, ErrUnboundReceipt) {
		t.Fatal("receipt without physical native thread accepted", err)
	}
	r.ObservedAt = r.ObservedAt.Add(time.Second)
	if err := g.IngestBoundReceipt(context.Background(), r); err != nil {
		t.Fatal("replayed receipt", err)
	}
	snap, err := g.Snapshot(context.Background())
	if err != nil || len(snap.Models) != 1 || snap.Models[0].Receipts != 1 {
		t.Fatal(snap, err)
	}
}

func TestOwnerControlsRefuseConcurrentPolicyReplacement(t *testing.T) {
	dir := t.TempDir()
	p := testPolicy()
	p.AuthorityPath = filepath.Join(dir, "authority.db")
	path := filepath.Join(dir, "policy.json")
	data, _ := json.Marshal(p)
	os.WriteFile(path, data, 0600)
	t.Setenv(PolicyEnv, path)
	g, err := Open(p.AuthorityPath, ReadPolicy, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.policy = func() (Policy, error) {
		old, e := ReadPolicy()
		if e != nil {
			return old, e
		}
		replacement := old
		replacement.TurnBudget = 80
		replacement.EmergencyStop = true
		b, _ := json.Marshal(replacement)
		if e = os.WriteFile(path, b, 0600); e != nil {
			return old, e
		}
		return old, nil
	}
	no := false
	if err = g.UpdateControls(context.Background(), Controls{Pause: &no}); err == nil {
		t.Fatal("concurrent owner edit overwritten")
	}
	saved, err := ReadPolicy()
	if err != nil || !saved.EmergencyStop || saved.TurnBudget != 80 {
		t.Fatal(saved, err)
	}
}

func TestObservedReceiptPreservesDriftAndStopsFutureDispatch(t *testing.T) {
	p := testPolicy()
	p.NativeAccountSHA256 = strings.Repeat("b", 64)
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	if err := g.VerifyAccount(ctx, p.NativeAccountSHA256); err != nil {
		t.Fatal(err)
	}
	req := request(t, g, "drift-attempt", "project", "implementation", "astra", "medium")
	req.Session = "session"
	req.NativeThreadID = "00000000-0000-4000-8000-000000000002"
	if err := g.RegisterNativeThread(ctx, req.Session, req.NativeThreadID); err != nil {
		t.Fatal(err)
	}
	if err := g.Dispatch(ctx, req, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, req.ID, "native-drift-turn"); err != nil {
		t.Fatal(err)
	}
	r := UsageReceipt{SourceKey: "codex:response:drift", ProviderTurnID: "native-drift-turn", NativeThreadID: req.NativeThreadID, Session: "untrusted-observer-placeholder", Model: "astra", Effort: "high", ObservedAt: now, InputTokens: 10, OutputTokens: 2, Confidence: "provider_response"}
	if err := g.IngestBoundReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	snap, err := g.Snapshot(ctx)
	if err != nil || !snap.PolicyStop || len(snap.Models) != 2 {
		t.Fatal(snap, err)
	}
	if err := g.Check(ctx); !errors.Is(err, ErrPolicy) {
		t.Fatal("policy drift did not block", err)
	}
	rows, err := g.SessionUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Effort == "high" {
			found = true
			if row.Session != "session" || row.Receipts != 1 || row.Input != 10 || row.Turns != 0 {
				t.Fatal(row)
			}
		}
	}
	if !found {
		t.Fatal("observed high receipt suppressed")
	}
}
