package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestRecordReviewPublicationPreservesImmutableVerdict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedProject(t, store, "pub")
	session, err := store.CreateSession(ctx, sampleRecord("pub"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	review := domain.Review{ID: "pub-review", SessionID: session.ID, ProjectID: session.ProjectID, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertReview(ctx, review); err != nil {
		t.Fatal(err)
	}
	run := domain.ReviewRun{ID: "pub-run", ReviewID: review.ID, SessionID: session.ID, Harness: domain.ReviewerCodex, PRURL: "https://github.com/o/r/pull/1", TargetSHA: "reviewed-sha", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, Body: "durable saved body", CreatedAt: now}
	if err := store.InsertReviewRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.RecordReviewPublication(ctx, run.ID, "wrong-sha", "991"); err != nil || ok {
		t.Fatalf("wrong commit accepted=%v err=%v", ok, err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := store.RecordReviewPublication(ctx, run.ID, run.TargetSHA, "991"); err != nil || !ok {
			t.Fatalf("receipt accepted=%v err=%v", ok, err)
		}
	}
	if ok, err := store.RecordReviewPublication(ctx, run.ID, run.TargetSHA, "992"); err != nil || ok {
		t.Fatalf("replacement receipt accepted=%v err=%v", ok, err)
	}
	got, ok, err := store.GetReviewRun(ctx, run.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Body != run.Body || got.Verdict != run.Verdict || got.TargetSHA != run.TargetSHA || got.Status != run.Status || got.GithubReviewID != "991" {
		t.Fatalf("saved result changed=%+v", got)
	}
}
