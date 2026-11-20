package nativereceipts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rewriteReceiptFixture(t *testing.T, path, data string) (os.FileInfo, os.FileInfo) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(0); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if _, err = f.WriteString(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	// Do not depend on timestamp resolution or sleep between fixture writes.
	changed := before.ModTime().Add(2 * time.Second)
	if err = os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fixture must rewrite the same file with changed mtime: %v", err)
	}
	return before, after
}

func TestObserverReplaysObservableFileRewrite(t *testing.T) {
	t.Run("same_size_replacement_after_append", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
		old := fixture(thread, "old", 100)
		if err := os.WriteFile(path, []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
		sink := &memorySink{}
		o := NewObserver([]string{dir}, sink)
		t.Cleanup(o.Close)
		bindings := []Binding{{"session", thread}}
		first := o.Sweep(t.Context(), bindings)
		if len(first.Errors) != 0 || len(sink.receipts) != 1 {
			t.Fatalf("initial receipt: %+v", first)
		}
		// Grow once before replacement: the comparison must use the latest
		// observation, not the FileInfo saved when this cursor was created.
		padding := record("response_item", map[string]any{"ignored": "padding"})
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(padding)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal(writeErr, closeErr)
		}
		o.Sweep(t.Context(), bindings)
		before, after := rewriteReceiptFixture(t, path, fixture(thread, "new", 101)+padding)
		if before.Size() != after.Size() || before.Size() <= int64(len(old)) {
			t.Fatal("fixture must preserve the latest, grown file size")
		}
		status := o.Sweep(t.Context(), bindings)
		r, ok := sink.receipts["codex:response:new"]
		if len(status.Errors) != 0 || !ok || len(sink.receipts) != 2 || r.InputTokens != 101 || r.Session != "session" || r.NativeThreadID != thread || r.ProviderTurnID != "root" {
			t.Errorf("same-size replacement lost its owned receipt: receipt=%+v count=%d status=%+v", r, len(sink.receipts), status)
		}
		// Restart replay must retain the old durable receipt and deduplicate
		// the replacement response through the existing sink contract.
		restarted := NewObserver([]string{dir}, sink)
		t.Cleanup(restarted.Close)
		replayed := restarted.Sweep(t.Context(), bindings)
		if len(replayed.Errors) != 0 || len(sink.receipts) != 2 {
			t.Fatalf("replacement restart replay: count=%d status=%+v", len(sink.receipts), replayed)
		}
	})

	t.Run("shrink_above_offset_rebuilds_model_context", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
		prefix := record("session_meta", map[string]any{"id": thread}) +
			record("turn_context", map[string]any{"turn_id": "root", "model": "gpt-6-astra", "effort": "medium"})
		partial := "{\"type\":\"response_item\",\"payload\":\"" + strings.Repeat("x", 2048)
		if err := os.WriteFile(path, []byte(prefix+partial), 0600); err != nil {
			t.Fatal(err)
		}
		sink := &memorySink{}
		o := NewObserver([]string{dir}, sink)
		t.Cleanup(o.Close)
		bindings := []Binding{{"session", thread}}
		first := o.Sweep(t.Context(), bindings)
		if !first.PendingBytes || len(first.Errors) != 0 || len(sink.receipts) != 0 || o.cursors[path].offset != int64(len(prefix)) {
			t.Fatalf("fixture did not stop after complete context: %+v", first)
		}
		newPrefix := strings.Replace(prefix, "gpt-6-astra", "gpt-6-terra", 1)
		receipt := record("token_usage_record", map[string]any{"thread_id": thread, "turn_id": "root", "root_turn_id": "root", "response_id": "shrink",
			"usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 90, "output_tokens": 20, "reasoning_output_tokens": 5}})
		before, after := rewriteReceiptFixture(t, path, newPrefix+receipt)
		if len(newPrefix) != len(prefix) || after.Size() >= before.Size() || after.Size() <= int64(len(prefix)) {
			t.Fatal("fixture must shrink while regrowing beyond the committed cursor")
		}
		status := o.Sweep(t.Context(), bindings)
		r, ok := sink.receipts["codex:response:shrink"]
		if len(status.Errors) != 0 || !ok || r.Model != "gpt-6-terra" || r.Effort != "medium" || r.InputTokens != 100 || r.Session != "session" || r.NativeThreadID != thread || r.ProviderTurnID != "root" {
			t.Fatalf("rewritten receipt used stale model context: receipt=%+v status=%+v", r, status)
		}
	})
}

func TestObserverPreservesCheckpointOnOrdinaryAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(fixture(thread, "first", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	o := NewObserver([]string{dir}, sink)
	t.Cleanup(o.Close)
	bindings := []Binding{{"session", thread}}
	first := o.Sweep(t.Context(), bindings)
	parser := o.cursors[path].parser
	unchanged := o.Sweep(t.Context(), bindings)
	if len(unchanged.Errors) != 0 || unchanged.BytesRead != first.BytesRead || unchanged.ObservedReceipts != first.ObservedReceipts || o.cursors[path].parser != parser {
		t.Fatalf("unchanged file reset its checkpoint: %+v", unchanged)
	}
	extra := record("token_usage_record", map[string]any{"thread_id": thread, "turn_id": "root", "root_turn_id": "root", "response_id": "appended",
		"usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 90, "output_tokens": 20, "reasoning_output_tokens": 5}})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(extra)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
	appended := o.Sweep(t.Context(), bindings)
	if len(appended.Errors) != 0 || len(sink.receipts) != 2 || appended.ObservedReceipts != first.ObservedReceipts+1 || o.cursors[path].parser != parser {
		t.Fatalf("ordinary append replayed earlier receipts or lost context: %+v", appended)
	}
}
