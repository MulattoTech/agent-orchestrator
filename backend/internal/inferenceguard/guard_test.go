package inferenceguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPolicy() Policy {
	roles := map[string]RolePolicy{}
	for _, r := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		roles[r] = RolePolicy{DefaultModel: "astra", Models: []string{"astra", "sol"}}
	}
	roles["lightweight"] = RolePolicy{DefaultModel: "sol", Models: []string{"sol", "luna"}}
	return Policy{Version: 1, AccountKey: "fake-account", AuthorizationEpoch: "fake-window", MaximumEffort: "medium", MaximumInference: 4, MaximumPerProject: 4, MaximumReviewers: 1, TurnBudget: 100, Roles: roles}
}
func openFake(t *testing.T, p *Policy, now *time.Time) *Gate {
	t.Helper()
	g, err := Open(filepath.Join(t.TempDir(), "guard.db"), func() (Policy, error) { return *p, nil }, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}
func request(t *testing.T, g *Gate, id, project, role, model, effort string) Request {
	t.Helper()
	s, err := g.Resolve(role, id, model, effort)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Session: id, Project: project, Operation: "turn/start", Selection: s}
}
func TestPolicyAllRolesAndEffortCeiling(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		for _, effort := range []string{"high", "xhigh", "ultra", "max", "unrecognized"} {
			t.Run(role+"/"+effort, func(t *testing.T) {
				if _, err := p.Resolve(role, "s", "astra", effort, now); !errors.Is(err, ErrPolicy) {
					t.Fatalf("got %v", err)
				}
			})
		}
	}
	if _, err := p.Resolve("reviewer", "review", "luna", "high", now); !errors.Is(err, ErrPolicy) {
		t.Fatal("Luna High reviewer bypass")
	}
	if _, err := p.Resolve("implementation", "worker", "luna", "medium", now); !errors.Is(err, ErrPolicy) {
		t.Fatal("Luna coding bypass")
	}
	if _, err := p.Resolve("lightweight", "short", "luna", "low", now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve("implementation", "s", "unsupported", "medium", now); !errors.Is(err, ErrPolicy) {
		t.Fatal("unsupported model accepted")
	}
	p.Exceptions = []Exception{{ID: "owner-exception", Owner: "owner", Session: "s", Model: "astra", Effort: "high", Reason: "bounded owner test", ExpiresAt: now.Add(time.Hour)}}
	if s, err := p.Resolve("implementation", "s", "astra", "high", now); err != nil || s.ExceptionID == "" {
		t.Fatal("valid owner exception refused")
	}
	if _, err := p.Resolve("implementation", "s", "astra", "high", now.Add(2*time.Hour)); !errors.Is(err, ErrPolicy) {
		t.Fatal("expired exception accepted")
	}
	if _, err := p.Resolve("implementation", "other", "astra", "high", now); !errors.Is(err, ErrPolicy) {
		t.Fatal("exception leaked to another session")
	}
}
func TestQuotaFortyDurableTasksRestartAndControlledResume(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "guard.db")
	open := func() *Gate {
		g, err := Open(path, func() (Policy, error) { return p, nil }, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := open()
	if err := g.Trip(ctx, now.Add(time.Hour), "fake-authoritative-reset"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	for i := 0; i < 40; i++ {
		r := request(t, g, fmt.Sprint(i), fmt.Sprint(i%3), "implementation", "sol", "medium")
		err := g.Dispatch(ctx, r, func() error { calls.Add(1); return nil })
		if !errors.Is(err, ErrQuota) {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	snap, err := g.Snapshot(ctx)
	if err != nil || snap.Queued != 40 || calls.Load() != 0 {
		t.Fatalf("snapshot=%+v calls=%d error=%v", snap, calls.Load(), err)
	}
	_ = g.Close()
	g = open()
	defer g.Close()
	now = now.Add(2 * time.Hour)
	r := request(t, g, "0", "0", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, r, func() error { calls.Add(1); return nil }); !errors.Is(err, ErrQuota) {
		t.Fatal("wall clock reset improperly resumed")
	}
	if err := g.ConfirmAllowance(ctx, "wrong-account", "fake-capacity", now, true); !errors.Is(err, ErrQuota) {
		t.Fatal("cross-account reset")
	}
	if err := g.ConfirmAllowance(ctx, p.AccountKey, "fake-capacity", now, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		id := fmt.Sprint(i)
		r := request(t, g, id, fmt.Sprint(i%3), "implementation", "sol", "medium")
		if err := g.Dispatch(ctx, r, func() error { calls.Add(1); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := g.Bind(ctx, id, "turn-"+id); err != nil {
			t.Fatal(err)
		}
		if err := g.Complete(ctx, id, "turn-"+id, ""); err != nil {
			t.Fatal(err)
		}
		if err := g.Dispatch(ctx, r, func() error { calls.Add(1); return nil }); !errors.Is(err, ErrUncertain) {
			t.Fatal("duplicate dispatch")
		}
	}
	snap, err = g.Snapshot(ctx)
	if err != nil || snap.Completed != 40 || snap.Queued != 0 || calls.Load() != 40 {
		t.Fatalf("%+v calls=%d err=%v", snap, calls.Load(), err)
	}
}
func TestSixtyFourContendersInferenceAndReviewerCaps(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	p.MaximumPerProject = 2
	now := time.Now()
	g := openFake(t, &p, &now)
	var calls atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	requests := make([]Request, 64)
	for i := range requests {
		role := "implementation"
		if i < 20 {
			role = "reviewer"
		}
		requests[i] = request(t, g, fmt.Sprint(i), fmt.Sprint(i%3), role, "astra", "medium")
	}
	for _, r := range requests {
		wg.Add(1)
		go func(r Request) {
			defer wg.Done()
			errs <- g.Dispatch(ctx, r, func() error { calls.Add(1); return nil })
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
	}
	s, err := g.Snapshot(ctx)
	if err != nil || s.Active != 4 || calls.Load() != 4 || s.Queued != 60 {
		t.Fatalf("%+v calls=%d %v", s, calls.Load(), err)
	}
	var reviews int
	if err := g.db.QueryRow("SELECT count(*) FROM attempts WHERE state='active' AND role='reviewer'").Scan(&reviews); err != nil || reviews > 1 {
		t.Fatalf("reviewers %d %v", reviews, err)
	}
}
func TestQuotaTripsBeforeAnotherAdmittedWrite(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	first := request(t, g, "first", "p", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, first, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Trip(ctx, time.Time{}, "fake-exhausted"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, role := range []string{"orchestrator", "implementation", "reviewer", "lightweight"} {
		r := request(t, g, role, "p", role, "sol", "medium")
		if err := g.Dispatch(ctx, r, func() error { calls++; return nil }); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("inference after quota stop")
	}
}
func TestUnknownQuotaBudgetAndCrashUncertainty(t *testing.T) {
	p := testPolicy()
	p.TurnBudget = 3
	p.ReserveTurns = 1
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		r := request(t, g, fmt.Sprint(i), "p", "implementation", "sol", "medium")
		if err := g.Dispatch(ctx, r, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		_ = g.SettleRequest(ctx, r.ID, "")
	}
	if err := g.Dispatch(ctx, request(t, g, "over", "p", "implementation", "sol", "medium"), func() error { t.Fatal("budget overrun"); return nil }); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	snap, _ := g.Snapshot(ctx)
	if snap.Quota != "UNKNOWN" {
		t.Fatal("fabricated remaining quota")
	}
	p.TurnBudget = 100
	r := request(t, g, "uncertain", "p", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, r, func() error { return errors.New("write interrupted") }); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	if err := g.Dispatch(ctx, r, func() error { t.Fatal("uncertain duplicate"); return nil }); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	snap, _ = g.Snapshot(ctx)
	if snap.Uncertain != 1 {
		t.Fatal("lost ambiguous lease")
	}
}
func TestImmutableIndependentReceipts(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	r := UsageReceipt{SourceKey: "response:1", ProviderTurnID: "turn1", Session: "s", Model: "sol", Effort: "medium", ObservedAt: now, InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3, ReasoningOutputTokens: 2, Confidence: "provider_response"}
	for i := 0; i < 10; i++ {
		if err := g.IngestReceipt(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	r.InputTokens = 11
	if err := g.IngestReceipt(ctx, r); err == nil {
		t.Fatal("mutable receipt")
	}
	r.SourceKey = "total:1"
	r.Confidence = "cumulative"
	if err := g.IngestReceipt(ctx, r); err == nil {
		t.Fatal("cumulative total accepted as receipt")
	}
	var n int
	_ = g.db.QueryRow("SELECT count(*) FROM usage_receipts").Scan(&n)
	if n != 1 {
		t.Fatalf("receipts %d", n)
	}
	if _, err := g.db.Exec("DELETE FROM usage_receipts"); err == nil {
		t.Fatal("immutable receipt deleted")
	}
}
func TestPolicyChangeAndMissingRequiredFileFailClosed(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	g := openFake(t, &p, &now)
	r := request(t, g, "id", "p", "implementation", "sol", "medium")
	p.Roles["implementation"] = RolePolicy{DefaultModel: "astra", Models: []string{"astra"}}
	if err := g.Dispatch(context.Background(), r, func() error { t.Fatal("stale model policy dispatched"); return nil }); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv(PolicyEnv, "")
	if err := CheckLaunch("codex", "chat"); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	t.Setenv(PolicyEnv, filepath.Join(t.TempDir(), "policy.json"))
	data, _ := json.Marshal(testPolicy())
	if err := os.WriteFile(os.Getenv(PolicyEnv), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch("claude", "chat"); !errors.Is(err, ErrPolicy) {
		t.Fatal("fallback provider")
	}
	if err := CheckLaunch("codex", "tui"); !errors.Is(err, ErrPolicy) {
		t.Fatal("terminal bypass")
	}
	if err := CheckLaunch("codex", "chat"); err != nil {
		t.Fatal(err)
	}
}

func TestSharedAccountAuthorityAcrossDataRootsAndExclusiveOwner(t *testing.T) {
	p := testPolicy()
	p.AuthorityPath = filepath.Join(t.TempDir(), "shared", "authority.db")
	path := filepath.Join(t.TempDir(), "policy.json")
	data, _ := json.Marshal(p)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PolicyEnv, path)
	a, err := ForDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := ForDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("two state roots obtained separate account authorities")
	}
	now := time.Now()
	if _, err := Open(p.AuthorityPath, func() (Policy, error) { return p, nil }, func() time.Time { return now }); !errors.Is(err, ErrPolicy) {
		t.Fatalf("second authority owner admitted: %v", err)
	}
	if err := a.Trip(context.Background(), time.Time{}, "fake-global-exhaustion"); err != nil {
		t.Fatal(err)
	}
	if err := b.Dispatch(context.Background(), request(t, b, "second-root", "project", "implementation", "sol", "medium"), func() error { t.Fatal("quota bypass from another state root"); return nil }); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
}
func TestNativeAccountBindingAndReceiptOnlySnapshot(t *testing.T) {
	p := testPolicy()
	p.NativeAccountSHA256 = AccountFingerprint("chatgpt", "fake@example.invalid")
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	if err := g.VerifyAccount(ctx, AccountFingerprint("chatgpt", "different@example.invalid")); !errors.Is(err, ErrPolicy) {
		t.Fatal("wrong native account")
	}
	if err := g.VerifyAccount(ctx, p.NativeAccountSHA256); err != nil {
		t.Fatal(err)
	}
	if err := g.IngestReceipt(ctx, UsageReceipt{SourceKey: "native-response", ProviderTurnID: "native-turn", Session: "session", Model: "historical-astra", Effort: "high", ObservedAt: now, InputTokens: 100, CachedInputTokens: 80, OutputTokens: 8, Confidence: "provider_response"}); err != nil {
		t.Fatal(err)
	}
	s, err := g.Snapshot(ctx)
	if err != nil || len(s.Models) != 1 || s.Models[0].Turns != 0 || s.Models[0].Receipts != 1 || s.Models[0].Input != 100 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestQueuedEpochAndIdentityCannotBypassBudget(t *testing.T) {
	p := testPolicy()
	p.TurnBudget, p.ReserveTurns = 2, 1
	p.Pause = true
	now := time.Now()
	g := openFake(t, &p, &now)
	ctx := context.Background()
	requests := []Request{request(t, g, "queued-one", "project", "implementation", "sol", "medium"), request(t, g, "queued-two", "project", "implementation", "sol", "medium")}
	for _, r := range requests {
		if err := g.Dispatch(ctx, r, func() error { t.Fatal("paused write"); return nil }); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	changed := requests[0]
	changed.Project = "different-project"
	if err := g.Dispatch(ctx, changed, func() error { t.Fatal("identity change"); return nil }); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	p.Pause = false
	p.AuthorizationEpoch = "owner-new-window"
	if err := g.Dispatch(ctx, requests[0], func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.SettleRequest(ctx, requests[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := g.Dispatch(ctx, requests[1], func() error { t.Fatal("old queued epoch bypassed budget"); return nil }); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	s, err := g.Snapshot(ctx)
	if err != nil || s.BudgetUsed != 1 || s.ConservativeTurnsRemaining != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}
func TestRestartRetainsAmbiguousLeaseAndKnownReset(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "guard.db")
	open := func() *Gate {
		g, e := Open(path, func() (Policy, error) { return p, nil }, func() time.Time { return now })
		if e != nil {
			t.Fatal(e)
		}
		return g
	}
	g := open()
	r := request(t, g, "active", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Hour)
	if err := g.Trip(ctx, reset, "authoritative-fixture"); err != nil {
		t.Fatal(err)
	}
	if err := g.Trip(ctx, time.Time{}, "later-unstructured-error"); err != nil {
		t.Fatal(err)
	}
	_ = g.Close()
	g = open()
	defer g.Close()
	s, err := g.Snapshot(ctx)
	if err != nil || s.Uncertain != 1 || s.Active != 0 || s.ResetAt != reset.UTC().Format(time.RFC3339Nano) || s.ResetSource != "authoritative-fixture" || !s.LatchedStop {
		t.Fatalf("%+v %v", s, err)
	}
	if err := g.Bind(ctx, r.ID, "native-turn"); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, r.ID, "different-turn"); err == nil {
		t.Fatal("native turn binding overwritten")
	}
	if err := g.Complete(ctx, r.Session, "native-turn", ""); err != nil {
		t.Fatal(err)
	}
	s, err = g.Snapshot(ctx)
	if err != nil || s.Uncertain != 0 || s.Completed != 1 {
		t.Fatalf("%+v %v", s, err)
	}
}
