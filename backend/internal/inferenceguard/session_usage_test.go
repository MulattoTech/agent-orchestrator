package inferenceguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func admitUsageTurn(t *testing.T, g *Gate, id, session, project, model, effort, nativeTurn string) {
	t.Helper()
	ctx := context.Background()
	r := request(t, g, id, project, "implementation", model, effort)
	r.Session = session
	if err := g.Dispatch(ctx, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, id, nativeTurn); err != nil {
		t.Fatal(err)
	}
}

func receiptUsage(t *testing.T, g *Gate, r UsageReceipt) {
	t.Helper()
	r.Confidence = "provider_response"
	if err := g.IngestReceipt(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestSessionUsageAggregatesIndependentFacts(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	base := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	now := base.Add(100 * time.Millisecond)
	g := openFake(t, &p, &now)

	admitUsageTurn(t, g, "first", "worker", "project", "sol", "medium", "native-first")
	if err := g.Complete(ctx, "worker", "native-first", ""); err != nil {
		t.Fatal(err)
	}
	one := UsageReceipt{SourceKey: "response-1", ProviderTurnID: "native-first", Session: "worker",
		Model: "sol", Effort: "medium", ObservedAt: now, InputTokens: 10, CachedInputTokens: 8, OutputTokens: 3, ReasoningOutputTokens: 1}
	receiptUsage(t, g, one)
	one.ObservedAt = now.Add(time.Hour)
	receiptUsage(t, g, one) // Immutable replay contributes no additional receipt.
	receiptUsage(t, g, UsageReceipt{SourceKey: "response-2", ProviderTurnID: "native-first", Session: "worker",
		Model: "sol", Effort: "medium", ObservedAt: base, InputTokens: 20, CachedInputTokens: 5, OutputTokens: 7, ReasoningOutputTokens: 2})

	// Clock correction and out-of-order ingestion must still select the exact
	// earliest timestamp, including 00Z before 00.1Z.
	now = base
	admitUsageTurn(t, g, "second", "worker", "project", "sol", "medium", "native-second")
	if err := g.Complete(ctx, "worker", "native-second", "provider_error"); err != nil {
		t.Fatal(err)
	}
	receiptUsage(t, g, UsageReceipt{SourceKey: "response-3", ProviderTurnID: "native-second", Session: "worker",
		Model: "sol", Effort: "medium", ObservedAt: base.Add(time.Second), InputTokens: 30, CachedInputTokens: 20, OutputTokens: 9, ReasoningOutputTokens: 4})

	now = base.Add(2 * time.Second)
	admitUsageTurn(t, g, "third", "worker", "project", "astra", "low", "native-third")
	if err := g.Complete(ctx, "worker", "native-third", ""); err != nil {
		t.Fatal(err)
	}
	receiptUsage(t, g, UsageReceipt{SourceKey: "response-4", ProviderTurnID: "native-third", Session: "worker",
		Model: "astra", Effort: "low", ObservedAt: now, InputTokens: 40, CachedInputTokens: 10, OutputTokens: 11, ReasoningOutputTokens: 5})

	r := request(t, g, "fourth", "project", "implementation", "sol", "low")
	r.Session = "worker"
	if err := g.Dispatch(ctx, r, func() error { return errors.New("synthetic ambiguous transport") }); !errors.Is(err, ErrUncertain) {
		t.Fatalf("expected uncertainty, got %v", err)
	}
	p.EmergencyStop = true
	queued := request(t, g, "only-queued", "project", "implementation", "sol", "medium")
	if err := g.Dispatch(ctx, queued, func() error { t.Fatal("queued dispatch wrote"); return nil }); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}

	rows, err := g.SessionUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected three admitted model/effort groups, got %+v", rows)
	}
	var turns, failed, uncertain, receipts int
	var input, cached, output, reasoning int64
	for _, row := range rows {
		if row.Session != "worker" || row.Project != "project" || row.ProjectAttribution != "recorded_governed_turn" {
			t.Fatalf("wrong attribution: %+v", row)
		}
		if row.BillableUnits != "UNKNOWN" || row.AllowanceUnits != "UNKNOWN" || row.DollarCost != "UNKNOWN" {
			t.Fatalf("invented billing: %+v", row)
		}
		turns += row.Turns
		failed += row.Failed
		uncertain += row.Uncertain
		receipts += row.Receipts
		input += row.Input
		cached += row.CachedInput
		output += row.Output
		reasoning += row.ReasoningOutput
		if row.Model == "sol" && row.Effort == "medium" {
			if row.Turns != 2 || row.Failed != 1 || row.Receipts != 3 || row.Input != 60 || row.CachedInput != 33 || row.Output != 19 || row.ReasoningOutput != 7 {
				t.Fatalf("multiple responses multiplied admissions or tokens: %+v", row)
			}
			if row.FirstDispatchAt == nil || !row.FirstDispatchAt.Equal(base) || row.FirstReceiptObservedAt == nil || !row.FirstReceiptObservedAt.Equal(base) {
				t.Fatalf("wrong earliest timestamps: %+v", row)
			}
		}
		if row.Model == "sol" && row.Effort == "low" {
			if row.Failed != 0 || row.Uncertain != 1 || row.Receipts != 0 || row.FirstReceiptObservedAt != nil {
				t.Fatalf("uncertain request was presented as a failed response: %+v", row)
			}
		}
	}
	if turns != 4 || failed != 1 || uncertain != 1 || receipts != 4 || input != 100 || cached != 43 || output != 30 || reasoning != 12 {
		t.Fatalf("incorrect totals: turns=%d failed=%d uncertain=%d receipts=%d input=%d cached=%d output=%d reasoning=%d",
			turns, failed, uncertain, receipts, input, cached, output, reasoning)
	}
	var attemptCount, receiptCount int
	if err := g.db.QueryRow("SELECT count(*) FROM attempts").Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err := g.db.QueryRow("SELECT count(*) FROM usage_receipts").Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 5 || receiptCount != 4 {
		t.Fatalf("read-only view rewrote raw evidence: attempts=%d receipts=%d", attemptCount, receiptCount)
	}
}

func TestSessionUsageRetainsReceiptOnlyHistoryWithoutInventingProject(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	g := openFake(t, &p, &now)
	admitUsageTurn(t, g, "known", "worker", "known-project", "sol", "medium", "known-turn")
	if err := g.Complete(ctx, "worker", "known-turn", ""); err != nil {
		t.Fatal(err)
	}
	receiptUsage(t, g, UsageReceipt{SourceKey: "historical-response", ProviderTurnID: "unrecorded-turn", Session: "worker",
		Model: "sol", Effort: "medium", ObservedAt: now, InputTokens: 50, CachedInputTokens: 20, OutputTokens: 12, ReasoningOutputTokens: 4})
	rows, err := g.SessionUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("receipt-only history missing or guessed a project: %+v", rows)
	}
	history := rows[0]
	if history.Project != "" || history.ProjectAttribution != "UNKNOWN" || history.Turns != 0 || history.Receipts != 1 ||
		history.Input != 50 || history.FirstDispatchAt != nil || history.FirstReceiptObservedAt == nil ||
		history.DispatchConfidence != "no_recorded_outer_turn" {
		t.Fatalf("historical attribution is not supported: %+v", history)
	}
	if rows[1].Project != "known-project" || rows[1].Turns != 1 || rows[1].Receipts != 0 {
		t.Fatalf("unrelated session turn absorbed history: %+v", rows[1])
	}
}

func TestSessionUsageAmbiguousProjectDoesNotMultiplyReceipts(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	g := openFake(t, &p, &now)
	for _, project := range []string{"project-a", "project-b"} {
		admitUsageTurn(t, g, project, "worker", project, "sol", "medium", "replayed-native-turn")
		if err := g.Complete(ctx, "worker", "replayed-native-turn", ""); err != nil {
			t.Fatal(err)
		}
	}
	receiptUsage(t, g, UsageReceipt{SourceKey: "one-native-response", ProviderTurnID: "replayed-native-turn", Session: "worker",
		Model: "sol", Effort: "medium", ObservedAt: now, InputTokens: 50, CachedInputTokens: 20, OutputTokens: 12, ReasoningOutputTokens: 4})
	rows, err := g.SessionUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected two project admissions plus unattributed response: %+v", rows)
	}
	var turns, receipts int
	var input int64
	for _, row := range rows {
		turns += row.Turns
		receipts += row.Receipts
		input += row.Input
		if row.Receipts > 0 && (row.Project != "" || row.ProjectAttribution != "UNKNOWN") {
			t.Fatalf("ambiguous project guessed: %+v", row)
		}
	}
	if turns != 2 || receipts != 1 || input != 50 {
		t.Fatalf("raw join multiplied evidence: turns=%d receipts=%d input=%d", turns, receipts, input)
	}
}
