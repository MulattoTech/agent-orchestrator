package nativereceipts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

// These fixtures use only temporary JSONL files and a synthetic account sink.
// Provider clocks must not stretch the five-minute unbound retry window.
func TestObserverFutureDatedUnboundReceiptDoesNotPersist(t *testing.T) {
	b := Binding{Session: "synthetic-session", Thread: thread}
	sink := newPrefixReceiptSink(b)
	sink.bound = false
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	data := prefixContext("gpt-6-astra") +
		strings.ReplaceAll(prefixUsage("future-unbound"), "2026-11-16T00:00:00Z", future)
	o, path, bindings := prefixObserver(t, data, sink)

	first := o.Sweep(t.Context(), bindings)
	if len(first.Errors) != 0 || first.BytesRead <= 0 || first.BytesRead > 8<<20 ||
		first.ObservedReceipts != 0 || first.UnattributedReceipts != 1 || sink.commits != 0 {
		t.Fatalf("unbound future receipt exceeded limits or committed: %+v commits=%d", first, sink.commits)
	}
	if len(o.cursors[path].parser.pending) != 0 {
		t.Fatal("future-dated unbound receipt became eligible for unlimited later replay")
	}

	sink.bound = true
	second := o.Sweep(t.Context(), bindings)
	if len(second.Errors) != 0 || second.BytesRead > 8<<20 || sink.commits != 0 {
		t.Fatalf("previously unbound future receipt was released later: %+v commits=%d", second, sink.commits)
	}
}

func TestParserPendingReceiptExpiryWindow(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		observedOffset         time.Duration
		cancelBefore           bool
		wantAfter              int
		wantError              bool
		boundInitially         bool
		shouldEventuallyCommit bool
	}{
		{name: "future_timestamp_pruned", observedOffset: 24 * time.Hour, wantAfter: 0},
		{name: "old_timestamp_pruned", observedOffset: -10 * time.Minute, wantAfter: 0},
		{name: "future_bound_receipt_never_commits", observedOffset: 24 * time.Hour, boundInitially: true, wantAfter: 0},
		{name: "expired_bound_receipt_never_commits", observedOffset: -10 * time.Minute, boundInitially: true, wantAfter: 0},
		{name: "recent_timestamp_remains_bounded", observedOffset: -time.Minute, wantAfter: 1, shouldEventuallyCommit: true},
		{name: "canceled_retry_preserves_unresolved", observedOffset: -time.Minute, cancelBefore: true, wantAfter: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Binding{Session: "synthetic-session", Thread: thread}
			sink := newPrefixReceiptSink(b)
			sink.bound = tc.boundInitially
			p := NewParser(b)
			r := inferenceguard.UsageReceipt{
				SourceKey: "codex:response:" + tc.name, Session: b.Session,
				NativeThreadID: b.Thread, ProviderTurnID: "root",
				ObservedAt: time.Now().Add(tc.observedOffset),
			}
			p.pending[r.SourceKey] = r
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			n, err := p.RetryPending(ctx, sink)
			if n != 0 || (err != nil) != tc.wantError {
				t.Fatalf("unexpected pending outcome count=%d err=%v", n, err)
			}
			if tc.wantError && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error missing: %v", err)
			}
			if len(p.pending) != tc.wantAfter || sink.commits != 0 {
				t.Fatalf("untrusted pending timestamp retained or discarded incorrectly: pending=%d want=%d commits=%d", len(p.pending), tc.wantAfter, sink.commits)
			}
			if tc.shouldEventuallyCommit {
				sink.bound = true
				accepted, err := p.RetryPending(t.Context(), sink)
				if err != nil || accepted != 1 || len(p.pending) != 0 || sink.commits != 1 {
					t.Fatalf("fresh pending binding did not converge exactly once: accepted=%d err=%v pending=%d commits=%d", accepted, err, len(p.pending), sink.commits)
				}
				again, err := p.RetryPending(t.Context(), sink)
				if err != nil || again != 0 || sink.commits != 1 {
					t.Fatalf("pending receipt replayed: accepted=%d err=%v commits=%d", again, err, sink.commits)
				}
			}
		})
	}
}
