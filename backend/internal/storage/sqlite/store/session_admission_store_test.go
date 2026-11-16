package store_test

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"testing"
	"time"
)

func TestSessionAdmissionQueueDurabilityFairnessAndFencing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	for _, id := range []string{"a1", "a2", "a3", "b1", "c1"} {
		rec := domain.SessionAdmissionRequest{ID: id, ProjectID: domain.ProjectID(id[:1]), Operation: "spawn", Payload: "{}", CreatedAt: now, UpdatedAt: now, AvailableAt: now}
		if err := s.EnqueueSessionAdmission(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueSessionAdmission(ctx, rec); err != nil {
			t.Fatal("idempotent enqueue", err)
		}
	}
	first, ok, err := s.ClaimSessionAdmission(ctx, "boot-1", now)
	if err != nil || !ok || first.ID != "a1" {
		t.Fatalf("first=%+v %v %v", first, ok, err)
	}
	first.State = "completed"
	first.UpdatedAt = now.Add(time.Second)
	if ok, err := s.CompleteSessionAdmission(ctx, first); err != nil || !ok {
		t.Fatal(ok, err)
	}
	second, ok, err := s.ClaimSessionAdmission(ctx, "boot-1", now.Add(time.Second))
	if err != nil || !ok || second.ID != "b1" {
		t.Fatalf("fairness=%+v %v %v", second, ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSessionAdmission(ctx, "boot-2", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	second.State = "completed"
	if ok, err := s.CompleteSessionAdmission(ctx, second); err != nil || ok {
		t.Fatalf("stale completion applied=%t err=%v", ok, err)
	}
	all, err := s.ListSessionAdmission(ctx)
	if err != nil || len(all) != 5 {
		t.Fatalf("lost requests %d %v", len(all), err)
	}
	completed := 0
	for _, rec := range all {
		if rec.State == "completed" {
			completed++
		}
		if rec.ID == "b1" && (rec.State != "queued" || rec.Generation <= second.Generation) {
			t.Fatalf("bad recovered claim %+v", rec)
		}
	}
	if completed != 1 {
		t.Fatalf("completed=%d", completed)
	}
	if ok, err := s.CancelSessionAdmission(ctx, "c1", now.Add(3*time.Second)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.CancelSessionAdmission(ctx, "a1", now); err != nil || ok {
		t.Fatal("completed command was cancellable", ok, err)
	}
	claim, ok, err := s.ClaimSessionAdmission(ctx, "boot-2", now.Add(4*time.Second))
	if err != nil || !ok || claim.ID == "c1" {
		t.Fatal(claim, ok, err)
	}
	if ok, err := s.CancelSessionAdmission(ctx, claim.ID, now); err != nil || ok {
		t.Fatal("running command was cancelled after admission", ok, err)
	}
}
