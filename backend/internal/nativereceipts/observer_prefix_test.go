package nativereceipts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

// prefixReceiptSink is a single synthetic account's immutable bound-receipt store.
// It opens no authority database and cannot invoke a provider.
type prefixReceiptSink struct {
	binding  Binding
	account  string
	bound    bool
	failNext bool
	receipts map[string]map[string]inferenceguard.UsageReceipt
	commits  int
}

func newPrefixReceiptSink(binding Binding) *prefixReceiptSink {
	return &prefixReceiptSink{binding: binding, account: "synthetic-account", bound: true,
		receipts: map[string]map[string]inferenceguard.UsageReceipt{}}
}

func (s *prefixReceiptSink) IngestBoundReceipt(ctx context.Context, r inferenceguard.UsageReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.account == "" || r.Session != s.binding.Session || r.NativeThreadID != s.binding.Thread || r.ProviderTurnID != "root" {
		return errors.New("synthetic account or native ownership mismatch")
	}
	if !s.bound {
		return inferenceguard.ErrUnboundReceipt
	}
	if s.failNext {
		s.failNext = false
		return errors.New("synthetic receipt store unavailable")
	}
	if s.receipts[s.account] == nil {
		s.receipts[s.account] = map[string]inferenceguard.UsageReceipt{}
	}
	if prior, ok := s.receipts[s.account][r.SourceKey]; ok {
		// Match the durable sink's timestamp-independent immutable payload rule.
		prior.ObservedAt = r.ObservedAt
		if prior != r {
			return errors.New("synthetic immutable receipt conflict")
		}
		return nil
	}
	s.receipts[s.account][r.SourceKey] = r
	s.commits++
	return nil
}

func (s *prefixReceiptSink) receipt(response string) (inferenceguard.UsageReceipt, bool) {
	r, ok := s.receipts[s.account]["codex:response:"+response]
	return r, ok
}

func prefixContext(model string) string {
	return record("session_meta", map[string]any{"id": thread}) +
		record("turn_context", map[string]any{"turn_id": "root", "model": model, "effort": "medium"})
}

func prefixUsage(response string) string {
	return record("token_usage_record", map[string]any{"thread_id": thread, "turn_id": "root", "root_turn_id": "root", "response_id": response,
		"usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 90, "output_tokens": 20, "reasoning_output_tokens": 5}})
}

func prefixPadding(t *testing.T, size int) string {
	t.Helper()
	empty := record("response_item", map[string]any{"padding": ""})
	if size < len(empty) {
		t.Fatal("fixture padding too short")
	}
	return record("response_item", map[string]any{"padding": strings.Repeat("x", size-len(empty))})
}

func prefixRewrite(t *testing.T, path, data string, preserveTime bool) (os.FileInfo, os.FileInfo) {
	t.Helper()
	before, after := rewriteReceiptFixture(t, path, data)
	if preserveTime {
		if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		var err error
		after, err = os.Stat(path)
		if err != nil || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("fixture did not preserve mtime", err)
		}
	}
	return before, after
}

func prefixObserver(t *testing.T, data string, sink *prefixReceiptSink) (*Observer, string, []Binding) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-prefix-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	o := NewObserver([]string{dir}, sink)
	t.Cleanup(o.Close)
	return o, path, []Binding{sink.binding}
}

func prefixAppend(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}

func TestObserverAuthenticatesPrefixBeforeCachedState(t *testing.T) {
	for _, scenario := range []string{"larger_regrowth", "same_size_same_mtime"} {
		t.Run(scenario, func(t *testing.T) {
			binding := Binding{"owned-session", thread}
			sink := newPrefixReceiptSink(binding)
			usage := prefixUsage("rewritten")
			initial := prefixContext("gpt-6-astra")
			if scenario == "same_size_same_mtime" {
				initial += prefixPadding(t, len(usage))
			}
			o, path, bindings := prefixObserver(t, initial, sink)
			first := o.Sweep(t.Context(), bindings)
			if len(first.Errors) != 0 || first.PendingBytes || sink.commits != 0 {
				t.Fatalf("initial context: %+v", first)
			}
			before, after := prefixRewrite(t, path, prefixContext("gpt-6-terra")+usage, scenario == "same_size_same_mtime")
			if !os.SameFile(before, after) || (scenario == "larger_regrowth" && after.Size() <= before.Size()) ||
				(scenario == "same_size_same_mtime" && after.Size() != before.Size()) {
				t.Fatal("fixture did not establish the intended unobserved rewrite")
			}
			status := o.Sweep(t.Context(), bindings)
			r, ok := sink.receipt("rewritten")
			if len(status.Errors) != 0 || !ok || r.Model != "gpt-6-terra" || r.Effort != "medium" || r.InputTokens != 100 || sink.commits != 1 {
				t.Fatalf("rewritten source did not rebuild owned context: receipt=%+v commits=%d status=%+v", r, sink.commits, status)
			}
			restarted := NewObserver([]string{filepath.Dir(path)}, sink)
			t.Cleanup(restarted.Close)
			replay := restarted.Sweep(t.Context(), bindings)
			if len(replay.Errors) != 0 || sink.commits != 1 {
				t.Fatalf("restart changed immutable receipt effects: %+v commits=%d", replay, sink.commits)
			}
		})
	}

	t.Run("pending_receipt_same_metadata", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		sink.bound = false
		now := time.Now().UTC().Format(time.RFC3339Nano)
		initial := strings.ReplaceAll(prefixContext("gpt-6-astra")+prefixUsage("pending"), "2026-11-16T00:00:00Z", now)
		o, path, bindings := prefixObserver(t, initial, sink)
		first := o.Sweep(t.Context(), bindings)
		if len(first.Errors) != 0 || first.UnattributedReceipts != 1 || len(o.cursors[path].parser.pending) != 1 || sink.commits != 0 {
			t.Fatalf("initial unbound pending receipt: %+v", first)
		}
		prefixRewrite(t, path, strings.Replace(initial, "gpt-6-astra", "gpt-6-terra", 1), true)
		sink.bound = true
		status := o.Sweep(t.Context(), bindings)
		r, ok := sink.receipt("pending")
		if len(status.Errors) != 0 || !ok || r.Model != "gpt-6-terra" || sink.commits != 1 || len(o.cursors[path].parser.pending) != 0 {
			t.Fatalf("stale pending context reached the account sink: receipt=%+v commits=%d status=%+v", r, sink.commits, status)
		}
	})

	t.Run("committed_receipt_remains_immutable", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		initial := prefixContext("gpt-6-astra") + prefixUsage("immutable")
		o, path, bindings := prefixObserver(t, initial, sink)
		if status := o.Sweep(t.Context(), bindings); len(status.Errors) != 0 || sink.commits != 1 {
			t.Fatalf("initial receipt: %+v", status)
		}
		prefixRewrite(t, path, strings.Replace(initial, "gpt-6-astra", "gpt-6-terra", 1), true)
		for range 2 {
			status := o.Sweep(t.Context(), bindings)
			r, ok := sink.receipt("immutable")
			if len(status.Errors) == 0 || !status.PendingBytes || !ok || r.Model != "gpt-6-astra" || sink.commits != 1 {
				t.Fatalf("conflicting rewrite disappeared or changed durable receipt: receipt=%+v commits=%d status=%+v", r, sink.commits, status)
			}
		}
	})

	t.Run("changed_native_thread_header_is_refused", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		usage := prefixUsage("foreign-header")
		initial := prefixContext("gpt-6-astra") + prefixPadding(t, len(usage))
		o, path, bindings := prefixObserver(t, initial, sink)
		if status := o.Sweep(t.Context(), bindings); len(status.Errors) != 0 {
			t.Fatalf("initial context: %+v", status)
		}
		foreign := "00000000-0000-4000-8000-999999999999"
		rewritten := strings.Replace(prefixContext("gpt-6-astra"), thread, foreign, 1) + usage
		before, after := prefixRewrite(t, path, rewritten, true)
		if before.Size() != after.Size() {
			t.Fatal("fixture must preserve size")
		}
		status := o.Sweep(t.Context(), bindings)
		if len(status.Errors) != 1 || status.Errors[0] != "native_thread_mismatch" || sink.commits != 0 {
			t.Fatalf("cached ownership hid a changed native header: %+v commits=%d", status, sink.commits)
		}
	})
}

func TestObserverPrefixVerificationMakesBoundedProgress(t *testing.T) {
	t.Run("verification_resumes_while_ordinary_appends_continue", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		padding := record("response_item", map[string]any{"padding": strings.Repeat("x", 3<<20)})
		initial := prefixContext("gpt-6-astra") + strings.Repeat(padding, 3)
		o, path, bindings := prefixObserver(t, initial, sink)
		var status SourceStatus
		for range 12 {
			status = o.Sweep(t.Context(), bindings)
			if len(status.Errors) != 0 || status.BytesRead > 8<<20 {
				t.Fatalf("initial bounded load: %+v", status)
			}
			if !status.PendingBytes {
				break
			}
		}
		if status.PendingBytes || o.cursors[path].offset != int64(len(initial)) || sink.commits != 0 {
			t.Fatalf("initial prefix did not converge: %+v", status)
		}
		parser := o.cursors[path].parser
		tail := prefixUsage("after-large-prefix")
		prefixAppend(t, path, tail)
		status = o.Sweep(t.Context(), bindings)
		if len(status.Errors) != 0 || !status.PendingBytes || status.BytesRead <= 0 || status.BytesRead > 8<<20 || sink.commits != 0 {
			t.Fatalf("unverified large prefix reached the sink: %+v commits=%d", status, sink.commits)
		}
		physical := status.BytesRead
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		canceled := o.Sweep(ctx, bindings)
		if canceled.BytesRead != 0 || sink.commits != 0 {
			t.Fatalf("canceled verification read or ingested: %+v", canceled)
		}
		for range 6 {
			extra := record("response_item", map[string]any{"padding": "ordinary append"})
			prefixAppend(t, path, extra)
			tail += extra
			status = o.Sweep(t.Context(), bindings)
			physical += status.BytesRead
			if len(status.Errors) != 0 || status.BytesRead > 8<<20 {
				t.Fatalf("bounded verification: %+v", status)
			}
			if !status.PendingBytes {
				break
			}
		}
		r, ok := sink.receipt("after-large-prefix")
		if status.PendingBytes || !ok || r.Model != "gpt-6-astra" || sink.commits != 1 || o.cursors[path].parser != parser {
			t.Fatalf("ordinary appends restarted or starved prefix verification: %+v receipt=%+v commits=%d", status, r, sink.commits)
		}
		if physical != int64(len(initial)+len(tail)) {
			t.Fatalf("prefix or continuation IO not charged exactly once: got=%d want=%d", physical, len(initial)+len(tail))
		}
	})

	t.Run("uncommitted_line_retry_is_not_hashed_twice", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		sink.failNext = true
		prefix := prefixContext("gpt-6-astra")
		o, path, bindings := prefixObserver(t, prefix+prefixUsage("retry"), sink)
		first := o.Sweep(t.Context(), bindings)
		if len(first.Errors) == 0 || !first.PendingBytes || o.cursors[path].offset != int64(len(prefix)) || sink.commits != 0 {
			t.Fatalf("fixture did not stop before the failed receipt: %+v", first)
		}
		parser := o.cursors[path].parser
		retried := o.Sweep(t.Context(), bindings)
		unchanged := o.Sweep(t.Context(), bindings)
		if len(retried.Errors) != 0 || len(unchanged.Errors) != 0 || retried.PendingBytes || unchanged.PendingBytes ||
			sink.commits != 1 || unchanged.ObservedReceipts != 1 || o.cursors[path].parser != parser {
			t.Fatalf("retry corrupted prefix identity or replayed the receipt: retried=%+v unchanged=%+v commits=%d", retried, unchanged, sink.commits)
		}
	})

	t.Run("zero_and_tiny_budgets_do_not_release_pending_receipts", func(t *testing.T) {
		binding := Binding{"owned-session", thread}
		sink := newPrefixReceiptSink(binding)
		sink.bound = false
		data := strings.ReplaceAll(prefixContext("gpt-6-astra")+prefixUsage("bounded-pending"), "2026-11-16T00:00:00Z", time.Now().UTC().Format(time.RFC3339Nano))
		o, path, bindings := prefixObserver(t, data, sink)
		if status := o.Sweep(t.Context(), bindings); len(status.Errors) != 0 || len(o.cursors[path].parser.pending) != 1 {
			t.Fatalf("initial pending fixture: %+v", status)
		}
		sink.bound = true
		for _, budget := range []int64{0, 1} {
			status := SourceStatus{}
			n, pending, err := o.read(t.Context(), path, binding, budget, &status)
			if err != nil || !pending || n < 0 || n > budget || status.ObservedReceipts != 0 || sink.commits != 0 {
				t.Fatalf("budget=%d used unverified pending receipt: bytes=%d pending=%t err=%v status=%+v commits=%d", budget, n, pending, err, status, sink.commits)
			}
		}
		if status := o.Sweep(t.Context(), bindings); len(status.Errors) != 0 || status.PendingBytes || sink.commits != 1 {
			t.Fatalf("adequate budget did not finish pending verification: %+v commits=%d", status, sink.commits)
		}
	})

	t.Run("binding_change_discards_incomplete_verification", func(t *testing.T) {
		binding := Binding{"old-session", thread}
		sink := newPrefixReceiptSink(binding)
		initial := prefixContext("gpt-6-astra") + prefixPadding(t, 1024)
		o, path, bindings := prefixObserver(t, initial, sink)
		if status := o.Sweep(t.Context(), bindings); len(status.Errors) != 0 || status.PendingBytes {
			t.Fatalf("initial context: %+v", status)
		}
		status := SourceStatus{}
		n, pending, err := o.read(t.Context(), path, binding, 256, &status)
		if err != nil || !pending || n <= 0 || n > 256 || sink.commits != 0 {
			t.Fatalf("fixture did not start a bounded verification: bytes=%d pending=%t err=%v", n, pending, err)
		}
		sink.binding = Binding{"new-session", thread}
		prefixAppend(t, path, prefixUsage("new-owner"))
		changed := o.Sweep(t.Context(), []Binding{sink.binding})
		r, ok := sink.receipt("new-owner")
		if len(changed.Errors) != 0 || changed.PendingBytes || !ok || r.Session != "new-session" || sink.commits != 1 {
			t.Fatalf("new binding retained old parser or verifier: %+v receipt=%+v", changed, r)
		}
	})
}
