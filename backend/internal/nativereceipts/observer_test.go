package nativereceipts

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memorySink struct {
	receipts map[string]inferenceguard.UsageReceipt
}

func (s *memorySink) IngestBoundReceipt(_ context.Context, r inferenceguard.UsageReceipt) error {
	if s.receipts == nil {
		s.receipts = map[string]inferenceguard.UsageReceipt{}
	}
	if old, ok := s.receipts[r.SourceKey]; ok && old.InputTokens != r.InputTokens {
		return fmt.Errorf("immutable conflict")
	}
	s.receipts[r.SourceKey] = r
	return nil
}

const thread = "00000000-0000-4000-8000-000000000001"

func record(kind string, payload any) string {
	b, _ := json.Marshal(map[string]any{"type": kind, "timestamp": "2026-11-16T00:00:00Z", "payload": payload})
	return string(b) + "\n"
}
func fixture(id, response string, input int) string {
	return record("session_meta", map[string]any{"id": id, "private": "must never reach sink"}) +
		record("turn_context", map[string]any{"turn_id": "root", "model": "gpt-6-astra", "effort": "medium"}) +
		record("token_usage_record", map[string]any{"thread_id": id, "turn_id": "internal", "root_turn_id": "root", "response_id": response,
			"usage":              map[string]any{"input_tokens": input, "cached_input_tokens": input - 10, "output_tokens": 20, "reasoning_output_tokens": 5},
			"thread_token_usage": map[string]any{"input_tokens": 9999999}, "turn_token_usage": map[string]any{"input_tokens": 9999999}})
}
func TestNativeResponseCountersIgnoreCumulativeAndBindExactContext(t *testing.T) {
	sink := &memorySink{}
	p := NewParser(Binding{"session", thread})
	for _, l := range strings.Split(strings.TrimSpace(fixture(thread, "response", 100)), "\n") {
		if _, e := p.Line(context.Background(), []byte(l), sink); e != nil {
			t.Fatal(e)
		}
	}
	r := sink.receipts["codex:response:response"]
	if r.InputTokens != 100 || r.CachedInputTokens != 90 || r.ProviderTurnID != "root" || r.Model != "gpt-6-astra" || r.Session != "session" {
		t.Fatalf("%+v", r)
	}
	for _, l := range strings.Split(strings.TrimSpace(fixture(thread, "response", 100)), "\n") {
		if _, e := p.Line(context.Background(), []byte(l), sink); e != nil {
			t.Fatal(e)
		}
	}
	if len(sink.receipts) != 1 {
		t.Fatal("duplicate receipt")
	}
}
func TestUnknownUsageDoesNotBecomeZero(t *testing.T) {
	sink := &memorySink{}
	p := NewParser(Binding{"s", thread})
	if _, e := p.Line(context.Background(), []byte(record("session_meta", map[string]any{"id": thread})), sink); e != nil {
		t.Fatal(e)
	}
	line := record("token_usage_record", map[string]any{"thread_id": thread, "response_id": "r", "turn_id": "t", "usage": map[string]any{"input_tokens": 1}})
	if _, e := p.Line(context.Background(), []byte(line), sink); e == nil {
		t.Fatal("missing measurements accepted")
	}
	if len(sink.receipts) != 0 {
		t.Fatal("invented receipt")
	}
}
func TestObserverFortyThreadsPartialAppendRestartAndForeignFile(t *testing.T) {
	dir := t.TempDir()
	sink := &memorySink{}
	var bindings []Binding
	for i := 1; i <= 40; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		bindings = append(bindings, Binding{fmt.Sprintf("s%d", i), id})
		path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
		if err := os.WriteFile(path, []byte(fixture(id, fmt.Sprintf("r%d", i), 100+i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	foreign := "00000000-0000-4000-8000-999999999999"
	if e := os.WriteFile(filepath.Join(dir, "rollout-test-"+foreign+".jsonl"), []byte(fixture(foreign, "foreign", 99)), 0600); e != nil {
		t.Fatal(e)
	}
	o := NewObserver([]string{dir}, sink)
	s := o.Sweep(context.Background(), bindings)
	if len(sink.receipts) != 40 || s.MatchedFiles != 40 || s.OwnedThreads != 40 || len(s.Errors) != 0 {
		t.Fatalf("receipts=%d status=%+v", len(sink.receipts), s)
	}
	p := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
	extra := record("token_usage_record", map[string]any{"thread_id": thread, "turn_id": "root", "root_turn_id": "root", "response_id": "partial",
		"usage": map[string]any{"input_tokens": 10, "cached_input_tokens": 0, "output_tokens": 2, "reasoning_output_tokens": 0}})
	f, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	f.WriteString(extra[:len(extra)-3])
	f.Close()
	s = o.Sweep(context.Background(), bindings)
	if len(sink.receipts) != 40 || !s.PendingBytes {
		t.Fatal("partial record consumed")
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(extra[len(extra)-3:])
	f.Close()
	o.Sweep(context.Background(), bindings)
	if len(sink.receipts) != 41 {
		t.Fatal("completed record lost")
	}
	restarted := NewObserver([]string{dir}, sink)
	restarted.Sweep(context.Background(), bindings)
	if len(sink.receipts) != 41 {
		t.Fatal("restart double counted")
	}
}
func TestObserverRejectsMismatchedNativeIdentityAndCancellation(t *testing.T) {
	dir := t.TempDir()
	sink := &memorySink{}
	p := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
	os.WriteFile(p, []byte(fixture("00000000-0000-4000-8000-999999999999", "bad", 100)), 0600)
	o := NewObserver([]string{dir}, sink)
	s := o.Sweep(context.Background(), []Binding{{"s", thread}})
	if len(s.Errors) == 0 || len(sink.receipts) != 0 {
		t.Fatal("foreign receipt accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	o.Sweep(ctx, []Binding{{"s", thread}})
	if time.Since(started) > time.Second {
		t.Fatal("cancellation ignored")
	}
}

type delayedBindingSink struct {
	memorySink
	bound bool
}

func (s *delayedBindingSink) IngestBoundReceipt(ctx context.Context, r inferenceguard.UsageReceipt) error {
	if !s.bound {
		return inferenceguard.ErrUnboundReceipt
	}
	return s.memorySink.IngestBoundReceipt(ctx, r)
}
func TestReceiptBeforeAcknowledgementRetriesWithoutReplayingFile(t *testing.T) {
	sink := &delayedBindingSink{}
	p := NewParser(Binding{"session", thread})
	data := strings.ReplaceAll(fixture(thread, "early", 100), "2026-11-16T00:00:00Z", time.Now().UTC().Format(time.RFC3339Nano))
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		_, _ = p.Line(context.Background(), []byte(line), sink)
	}
	if len(sink.receipts) != 0 {
		t.Fatal("unbound receipt attributed")
	}
	sink.bound = true
	n, err := p.RetryPending(context.Background(), sink)
	if err != nil || n != 1 || len(sink.receipts) != 1 {
		t.Fatal(n, err, len(sink.receipts))
	}
	n, err = p.RetryPending(context.Background(), sink)
	if err != nil || n != 0 {
		t.Fatal("pending duplicate", n, err)
	}
}
func TestConflictingThreadOwnershipCannotBeReintroducedByThirdBinding(t *testing.T) {
	dir := t.TempDir()
	sink := &memorySink{}
	os.WriteFile(filepath.Join(dir, "rollout-test-"+thread+".jsonl"), []byte(fixture(thread, "r", 100)), 0600)
	s := NewObserver([]string{dir}, sink).Sweep(context.Background(), []Binding{{"a", thread}, {"b", thread}, {"a", thread}})
	if s.OwnedThreads != 0 || len(sink.receipts) != 0 || len(s.Errors) == 0 {
		t.Fatalf("%+v", s)
	}
}

func TestObserverChargesPartialRecordsToSweepBudgetAndDoesNotStarveFinalFile(t *testing.T) {
	dir := t.TempDir()
	sink := &memorySink{}
	var bindings []Binding
	prefixLengths := map[string]int64{}
	for i := 1; i <= 9; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		bindings = append(bindings, Binding{fmt.Sprintf("s%d", i), id})
		prefix := record("session_meta", map[string]any{"id": id}) +
			record("turn_context", map[string]any{"turn_id": "root", "model": "gpt-6-astra", "effort": "medium"})
		path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
		prefixLengths[path] = int64(len(prefix))
		// Eight writers have unfinished records; the ninth has one complete
		// receipt. Charging only complete lines permits ~36MiB per sweep.
		line := `{"type":"response_item","payload":"` + strings.Repeat("x", MaxLineBytes-1024)
		if i == 9 {
			line = record("token_usage_record", map[string]any{"thread_id": id, "turn_id": "root", "root_turn_id": "root", "response_id": "last-file",
				"usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 90, "output_tokens": 20, "reasoning_output_tokens": 5}, "ignored_padding": strings.Repeat("x", MaxLineBytes-1024)})
		}
		if err := os.WriteFile(path, []byte(prefix+line), 0600); err != nil {
			t.Fatal(err)
		}
	}
	o := NewObserver([]string{dir}, sink)
	first := o.Sweep(t.Context(), bindings)
	if first.BytesRead != 32<<20 || !first.PendingBytes || len(sink.receipts) != 0 {
		t.Fatalf("physical read budget was not enforced: %+v receipts=%d", first, len(sink.receipts))
	}
	for path, cursor := range o.cursors {
		if cursor.offset != prefixLengths[path] {
			t.Fatalf("partial JSON advanced durable replay position: %s offset=%d want=%d", path, cursor.offset, prefixLengths[path])
		}
	}
	second := o.Sweep(t.Context(), bindings)
	if second.BytesRead > 32<<20 || len(sink.receipts) != 1 || sink.receipts["codex:response:last-file"].InputTokens != 100 {
		t.Fatalf("unfinished files starved the final complete receipt: %+v receipts=%d", second, len(sink.receipts))
	}
}

func TestObserverChargesMalformedAndOversizedReadsWithoutAdvancingCheckpoint(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			prefix := record("session_meta", map[string]any{"id": thread})
			bad := "{not JSON}\n" + strings.Repeat("x", 128<<10)
			minimumRead := int64(len(prefix) + len("{not JSON}\n"))
			if scenario == "oversized" {
				bad = strings.Repeat("x", MaxLineBytes+1) + "\n"
				minimumRead = int64(len(prefix) + MaxLineBytes + 1)
			}
			path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
			if err := os.WriteFile(path, []byte(prefix+bad), 0600); err != nil {
				t.Fatal(err)
			}
			o := NewObserver([]string{dir}, &memorySink{})
			status := SourceStatus{}
			n, pending, err := o.read(t.Context(), path, Binding{"s", thread}, 8<<20, &status)
			if err == nil || !pending || n < minimumRead || n > int64(len(prefix+bad)) {
				t.Fatalf("failed record IO vanished from accounting: bytes=%d min=%d pending=%t err=%v", n, minimumRead, pending, err)
			}
			if o.cursors[path].offset != int64(len(prefix)) {
				t.Fatal("failed record was consumed")
			}
		})
	}
}
