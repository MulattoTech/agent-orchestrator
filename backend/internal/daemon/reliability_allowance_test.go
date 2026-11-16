package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type noAllowanceOpen struct{ opens int }

func (f *noAllowanceOpen) Open(context.Context, ports.CodexAccountContext) (ports.CodexAccountClient, error) {
	f.opens++
	panic("native client must not launch")
}
func (f *noAllowanceOpen) Capabilities(context.Context) domain.CodexAccountCapabilities {
	panic("capability probe forbidden")
}
func TestReliabilityAllowanceDisabledBeforeNativeLaunch(t *testing.T) {
	t.Setenv("AO_ENABLE_ALLOWANCE_CHECK", "")
	f := &noAllowanceOpen{}
	if err := verifyPostRefreshAllowance(context.Background(), nil, f, t.TempDir()); err == nil || f.opens != 0 {
		t.Fatal("disabled check launched native process")
	}
}
func TestReliabilityAllowanceRequiresFreshCompleteWindows(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	short, weekly := int64(300), int64(10080)
	valid := func() ports.CodexCapacityObservation {
		return ports.CodexCapacityObservation{ObservedAt: now, Overall: &domain.CodexCapacityBucket{Reached: domain.CodexCapacityNotReached, Primary: &domain.CodexCapacityWindow{UsedPercent: 20, WindowDurationMinutes: &short, ResetsAt: &later}, Secondary: &domain.CodexCapacityWindow{UsedPercent: 40, WindowDurationMinutes: &weekly, ResetsAt: &later}}}
	}
	if err := completeAvailableCapacity(valid(), now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	t.Run("complete weekly first", func(t *testing.T) {
		c := valid()
		c.Overall.Primary, c.Overall.Secondary = c.Overall.Secondary, c.Overall.Primary
		if err := completeAvailableCapacity(c, now.Add(-time.Second), now); err != nil {
			t.Fatal("qualified native weekly-first layout rejected:", err)
		}
	})
	cases := map[string]func(*ports.CodexCapacityObservation){
		"partial":         func(c *ports.CodexCapacityObservation) { c.Partial = true },
		"unknown":         func(c *ports.CodexCapacityObservation) { c.Overall.Reached = domain.CodexCapacityReachUnknown },
		"exhausted":       func(c *ports.CodexCapacityObservation) { c.Overall.Secondary.UsedPercent = 100 },
		"missing weekly":  func(c *ports.CodexCapacityObservation) { c.Overall.Secondary = nil },
		"old window":      func(c *ports.CodexCapacityObservation) { c.Overall.Primary.ResetsAt = &now },
		"stale":           func(c *ports.CodexCapacityObservation) { c.ObservedAt = now.Add(-time.Hour) },
		"nan":             func(c *ports.CodexCapacityObservation) { c.Overall.Primary.UsedPercent = math.NaN() },
		"duplicate short": func(c *ports.CodexCapacityObservation) { c.Overall.Secondary.WindowDurationMinutes = &short },
		"duplicate weekly": func(c *ports.CodexCapacityObservation) {
			c.Overall.Primary.WindowDurationMinutes = &weekly
		},
		"recorded weekly-only layout": func(c *ports.CodexCapacityObservation) {
			c.Overall.Primary, c.Overall.Secondary = c.Overall.Secondary, nil
		},
		"unknown ordered layout": func(c *ports.CodexCapacityObservation) {
			day := int64(1440)
			c.Overall.Secondary.WindowDurationMinutes = &day
		},
		"missing duration": func(c *ports.CodexCapacityObservation) { c.Overall.Secondary.WindowDurationMinutes = nil },
		"additional duplicate short": func(c *ports.CodexCapacityObservation) {
			c.AdditionalBuckets = []domain.CodexCapacityBucket{{Reached: domain.CodexCapacityNotReached, Primary: c.Overall.Primary, Secondary: c.Overall.Primary}}
		},
		"additional partial layout": func(c *ports.CodexCapacityObservation) {
			c.AdditionalBuckets = []domain.CodexCapacityBucket{{Reached: domain.CodexCapacityNotReached, Primary: c.Overall.Primary}}
		},
		"another bucket unknown": func(c *ports.CodexCapacityObservation) {
			c.AdditionalBuckets = []domain.CodexCapacityBucket{{Reached: domain.CodexCapacityReachUnknown}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			if completeAvailableCapacity(c, now.Add(-time.Second), now) == nil {
				t.Fatal("unproven allowance accepted")
			}
		})
	}
}
func TestReliabilityAllowanceRejectsAPIKeyOrUnknownIdentity(t *testing.T) {
	email := "fixture@example.invalid"
	o := ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email}
	if _, err := allowanceIdentity(o); err != nil {
		t.Fatal(err)
	}
	o.Method = domain.CodexAuthMethodAPIKey
	if _, err := allowanceIdentity(o); err == nil {
		t.Fatal("account substitution accepted")
	}
}

type fixtureAllowanceClient struct {
	calls          *[]string
	email          string
	mutateCapacity func(*ports.CodexCapacityObservation)
}

func (c *fixtureAllowanceClient) Read(context.Context, bool) (ports.CodexAccountObservation, error) {
	*c.calls = append(*c.calls, "account/read")
	return ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &c.email}, nil
}
func (c *fixtureAllowanceClient) ReadCapacity(context.Context) (ports.CodexCapacityObservation, error) {
	*c.calls = append(*c.calls, "account/rateLimits/read")
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	short := int64(300)
	long := int64(10080)
	capacity := ports.CodexCapacityObservation{ObservedAt: now, Overall: &domain.CodexCapacityBucket{Reached: domain.CodexCapacityNotReached, Primary: &domain.CodexCapacityWindow{UsedPercent: 10, WindowDurationMinutes: &short, ResetsAt: &reset}, Secondary: &domain.CodexCapacityWindow{UsedPercent: 20, WindowDurationMinutes: &long, ResetsAt: &reset}}}
	if c.mutateCapacity != nil {
		c.mutateCapacity(&capacity)
	}
	return capacity, nil
}
func (c *fixtureAllowanceClient) Close() error                 { *c.calls = append(*c.calls, "close"); return nil }
func (c *fixtureAllowanceClient) Logout(context.Context) error { panic("logout forbidden") }
func (c *fixtureAllowanceClient) ReadUsage(context.Context) (ports.CodexUsageObservation, error) {
	panic("unneeded usage call")
}
func (c *fixtureAllowanceClient) ConsumeResetCredit(context.Context, string) (domain.CodexResetCreditOutcome, error) {
	panic("credit consumption forbidden")
}
func (c *fixtureAllowanceClient) Events() <-chan ports.CodexAccountEvent { panic("unneeded events") }

type fixtureAllowanceFactory struct{ client *fixtureAllowanceClient }

func (f fixtureAllowanceFactory) Open(context.Context, ports.CodexAccountContext) (ports.CodexAccountClient, error) {
	*f.client.calls = append(*f.client.calls, "open")
	return f.client, nil
}
func (f fixtureAllowanceFactory) Capabilities(context.Context) domain.CodexAccountCapabilities {
	panic("capability process probe forbidden")
}
func fakeReliabilityGate(t *testing.T, emergencyStop bool) (*inferenceguard.Gate, string, string) {
	t.Helper()
	dir := t.TempDir()
	email := "fixture@example.invalid"
	roles := map[string]inferenceguard.RolePolicy{}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		roles[role] = inferenceguard.RolePolicy{DefaultModel: "astra", Models: []string{"astra"}}
	}
	p := inferenceguard.Policy{Version: 1, AccountKey: "fixture", AuthorizationEpoch: "fixture", AuthorityPath: filepath.Join(dir, "authority.db"), NativeAccountSHA256: inferenceguard.AccountFingerprint("chatgpt", email), EmergencyStop: emergencyStop, MaximumEffort: "medium", MaximumInference: 1, MaximumPerProject: 1, MaximumReviewers: 1, TurnBudget: 3, ReserveTurns: 1, Roles: roles}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(inferenceguard.PolicyEnv, path)
	g, err := inferenceguard.Open(p.AuthorityPath, inferenceguard.ReadPolicy, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g, dir, email
}

func TestReliabilityAllowanceBridgeUsesOnlyAccountReadsAndKeepsEmergencyStop(t *testing.T) {
	t.Setenv("AO_ENABLE_ALLOWANCE_CHECK", "1")
	cases := []struct {
		name        string
		weeklyFirst bool
		invalid     bool
	}{
		{name: "qualified-short-first"},
		{name: "qualified-weekly-first", weeklyFirst: true},
		{name: "duplicate-short-keeps-exhausted", invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, dir, email := fakeReliabilityGate(t, true)
			if err := g.Trip(context.Background(), time.Time{}, "fixture exhausted"); err != nil {
				t.Fatal(err)
			}
			var calls []string
			client := &fixtureAllowanceClient{calls: &calls, email: email}
			if tc.weeklyFirst {
				client.mutateCapacity = func(c *ports.CodexCapacityObservation) {
					c.Overall.Primary, c.Overall.Secondary = c.Overall.Secondary, c.Overall.Primary
				}
			}
			if tc.invalid {
				client.mutateCapacity = func(c *ports.CodexCapacityObservation) {
					c.Overall.Secondary.WindowDurationMinutes = c.Overall.Primary.WindowDurationMinutes
				}
			}
			err := verifyPostRefreshAllowance(context.Background(), g, fixtureAllowanceFactory{client}, dir)
			if tc.invalid && err == nil {
				t.Fatal("duplicate short window cleared exhaustion")
			}
			if !tc.invalid && err != nil {
				t.Fatal(err)
			}
			if strings.Join(calls, ",") != "open,account/read,account/rateLimits/read,account/read,close" {
				t.Fatal(calls)
			}
			s, err := g.Snapshot(context.Background())
			wantQuota := "AVAILABLE"
			if tc.invalid {
				wantQuota = "EXHAUSTED"
			}
			if err != nil || s.Quota != wantQuota || !s.EmergencyStop || s.BudgetUsed != 0 {
				t.Fatal(s, err)
			}
			if err = g.Check(context.Background()); err == nil {
				t.Fatal("allowance verification silently resumed emergency-stopped work")
			}
		})
	}
}

func TestReliabilityEmergencyStopPersistsWhenNativeInterruptFails(t *testing.T) {
	g, _, _ := fakeReliabilityGate(t, false)
	syntheticFailure := errors.New("synthetic native interruption failure")
	var calls atomic.Int32
	unregister := g.RegisterStopper(func(context.Context) error {
		calls.Add(1)
		p, err := inferenceguard.ReadPolicy()
		if err != nil {
			return err
		}
		if !p.EmergencyStop {
			return errors.New("native interruption attempted before stop persisted")
		}
		return syntheticFailure
	})
	defer unregister()
	stop := true
	err := updateReliabilityInferenceControls(context.Background(), g, inferenceguard.Controls{EmergencyStop: &stop})
	if !errors.Is(err, syntheticFailure) || !strings.Contains(err.Error(), "emergency stop persisted") ||
		!strings.Contains(err.Error(), "not fully acknowledged") || calls.Load() != 1 {
		t.Fatalf("persisted stop or native failure hidden: calls=%d err=%v", calls.Load(), err)
	}
	s, err := g.Snapshot(context.Background())
	if err != nil || !s.EmergencyStop || s.BudgetUsed != 0 {
		t.Fatal(s, err)
	}
	if err := g.Check(context.Background()); err == nil {
		t.Fatal("failed native interruption rolled back emergency stop")
	}
}
