package daemon

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// verifyPostRefreshAllowance is an explicit operator action, disabled by default.
// It uses account/read and account/rateLimits/read only. It never opens a thread,
// starts a turn, consumes reset credits, switches accounts, or probes capabilities.
// The actual native adapter remains a post-refresh qualification gate.
func verifyPostRefreshAllowance(ctx context.Context, g *inferenceguard.Gate, factory ports.CodexAccountClientFactory, accountHome string) error {
	if os.Getenv("AO_ENABLE_ALLOWANCE_CHECK") != "1" {
		return errors.New("native allowance check is disabled; enable only for the controlled post-refresh validation")
	}
	p, err := inferenceguard.ReadPolicy()
	if err != nil {
		return err
	}
	if p.NativeAccountSHA256 == "" || factory == nil || !filepath.IsAbs(accountHome) {
		return errors.New("verified owner account binding and absolute native home required")
	}
	if !p.EmergencyStop {
		return errors.New("enable emergency stop before verifying native allowance")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now().UTC()
	client, err := factory.Open(ctx, ports.CodexAccountContext{Home: accountHome})
	if err != nil {
		return errors.New("native account observation unavailable")
	}
	defer client.Close()
	before, err := client.Read(ctx, false)
	if err != nil {
		return errors.New("native identity observation failed")
	}
	fingerprint, err := allowanceIdentity(before)
	if err != nil {
		return err
	}
	if err = g.VerifyAccount(ctx, fingerprint); err != nil {
		return err
	}
	capacity, err := client.ReadCapacity(ctx)
	if err != nil {
		return errors.New("native allowance observation failed")
	}
	after, err := client.Read(ctx, false)
	if err != nil {
		return errors.New("native identity recheck failed")
	}
	afterFingerprint, err := allowanceIdentity(after)
	if err != nil || afterFingerprint != fingerprint {
		return errors.New("native account changed during allowance observation")
	}
	if err = g.VerifyAccount(ctx, afterFingerprint); err != nil {
		return err
	}
	if err = completeAvailableCapacity(capacity, started, time.Now().UTC()); err != nil {
		return err
	}
	return g.ConfirmAllowance(ctx, p.AccountKey, "native account/rateLimits/read; complete windows; stable verified identity", capacity.ObservedAt, true)
}
func allowanceIdentity(o ports.CodexAccountObservation) (string, error) {
	if o.Authentication != domain.AgentAuthenticationAuthorized || o.Method != domain.CodexAuthMethodChatGPT || o.Email == nil || *o.Email == "" {
		return "", errors.New("authenticated owner ChatGPT identity is not established")
	}
	return inferenceguard.AccountFingerprint("chatgpt", *o.Email), nil
}
func completeAvailableCapacity(c ports.CodexCapacityObservation, started, now time.Time) error {
	if c.Partial || c.Overall == nil || c.ObservedAt.Before(started) || c.ObservedAt.After(now) || now.Sub(c.ObservedAt) > time.Minute {
		return errors.New("allowance observation is partial, stale, or missing")
	}
	buckets := append([]domain.CodexCapacityBucket{*c.Overall}, c.AdditionalBuckets...)
	for _, b := range buckets {
		if b.Reached != domain.CodexCapacityNotReached {
			return errors.New("an allowance bucket is exhausted or unknown")
		}
		// This candidate qualifies only a complete five-hour/weekly pair.
		// Native primary/secondary order is not stable: recorded native
		// evidence places the weekly window first. Duplicate, missing or
		// unfamiliar windows cannot prove that both allowances refreshed.
		// Additional model buckets require the same qualified pair.
		if b.Primary == nil || b.Secondary == nil ||
			b.Primary.WindowDurationMinutes == nil || b.Secondary.WindowDurationMinutes == nil {
			return errors.New("both qualified short and long allowance windows are required")
		}
		short, weekly := *b.Primary.WindowDurationMinutes, *b.Secondary.WindowDurationMinutes
		if short > weekly {
			short, weekly = weekly, short
		}
		if short != 300 || weekly != 10080 {
			return errors.New("unqualified native allowance layout; expected distinct five-hour and weekly windows")
		}
		for _, w := range []*domain.CodexCapacityWindow{b.Primary, b.Secondary} {
			if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent >= 100 || w.ResetsAt == nil || !w.ResetsAt.After(now) {
				return errors.New("an allowance window does not prove current availability")
			}
		}
	}
	return nil
}
