package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func publicationClaimStore(t *testing.T) (*sqlite.Store, domain.ReviewRun) {
	t.Helper()
	s := newTestStore(t)
	seedProject(t, s, "publication")
	ctx := context.Background()
	worker, err := s.CreateSession(ctx, sampleRecord("publication"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reviewer := domain.Review{ID: "claim-review", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}
	if err := s.UpsertReview(ctx, reviewer); err != nil {
		t.Fatal(err)
	}
	run := domain.ReviewRun{ID: "claim-run", ReviewID: reviewer.ID, SessionID: worker.ID, Harness: reviewer.Harness,
		PRURL: "https://github.com/o/r/pull/1", TargetSHA: "1111111111111111111111111111111111111111",
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, Body: "Immutable review body", CreatedAt: now}
	if err := s.InsertReviewRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return s, run
}

func TestPublicationClaimBindsExactResultAndFencesRelease(t *testing.T) {
	s, run := publicationClaimStore(t)
	ctx := context.Background()
	first, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now())
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	second, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now())
	if err != nil || claimed || second.ClaimID != first.ClaimID || second.AttemptCount != 1 {
		t.Fatalf("duplicate claim=%v attempt=%+v err=%v", claimed, second, err)
	}
	cases := map[string]func(*domain.ReviewRun){
		"body":         func(r *domain.ReviewRun) { r.Body += " changed" },
		"verdict":      func(r *domain.ReviewRun) { r.Verdict = domain.VerdictChangesRequested },
		"commit":       func(r *domain.ReviewRun) { r.TargetSHA = "different" },
		"worker":       func(r *domain.ReviewRun) { r.SessionID = "different" },
		"pull request": func(r *domain.ReviewRun) { r.PRURL = "https://github.com/o/r/pull/2" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			other := run
			change(&other)
			if _, claimed, err := s.ClaimReviewPublication(ctx, other, time.Now()); claimed || !errors.Is(err, domain.ErrReviewPublicationConflict) {
				t.Fatalf("binding changed claimed=%v err=%v", claimed, err)
			}
		})
	}
	if released, err := s.ReleaseReviewPublicationPrewrite(ctx, run.ID, "wrong-owner"); err != nil || released {
		t.Fatalf("foreign release=%v err=%v", released, err)
	}
	if released, err := s.ReleaseReviewPublicationPrewrite(ctx, run.ID, first.ClaimID); err != nil || !released {
		t.Fatalf("pre-write release=%v err=%v", released, err)
	}
	third, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now())
	if err != nil || !claimed || third.ClaimID == first.ClaimID || third.AttemptCount != 2 {
		t.Fatalf("new safe claim=%v %+v err=%v", claimed, third, err)
	}
	if released, err := s.ReleaseReviewPublicationPrewrite(ctx, run.ID, first.ClaimID); err != nil || released {
		t.Fatalf("stale release=%v err=%v", released, err)
	}
}

func TestPublicationReceiptCommitsAtomicallyAndRejectsRebinding(t *testing.T) {
	s, run := publicationClaimStore(t)
	ctx := context.Background()
	if _, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now()); err != nil || !claimed {
		t.Fatal(err)
	}
	receipt := ports.SCMReviewPublicationReceipt{ReviewID: "991", TargetSHA: run.TargetSHA, State: "COMMENTED", URL: "https://github.com/o/r/pull/1#pullrequestreview-991"}
	changed := run
	changed.Body = "another body"
	if ok, err := s.CompleteReviewPublication(ctx, changed, receipt, time.Now()); ok || !errors.Is(err, domain.ErrReviewPublicationConflict) {
		t.Fatalf("wrong body receipt=%v err=%v", ok, err)
	}
	got, _, err := s.GetReviewRun(ctx, run.ID)
	if err != nil || got.GithubReviewID != "" || got.Body != run.Body {
		t.Fatalf("partial receipt commit: %+v %v", got, err)
	}
	for range 2 {
		if ok, err := s.CompleteReviewPublication(ctx, run, receipt, time.Now()); err != nil || !ok {
			t.Fatalf("receipt=%v err=%v", ok, err)
		}
	}
	attempt, _, err := s.GetReviewPublicationAttempt(ctx, run.ID)
	if err != nil || attempt.State != "published" || attempt.ProviderReviewID != "991" || attempt.CompletedAt == nil {
		t.Fatalf("terminal receipt=%+v err=%v", attempt, err)
	}
	replacement := receipt
	replacement.ReviewID = "992"
	if ok, err := s.CompleteReviewPublication(ctx, run, replacement, time.Now()); ok || !errors.Is(err, domain.ErrReviewPublicationConflict) {
		t.Fatalf("replacement=%v err=%v", ok, err)
	}
	if released, err := s.ReleaseReviewPublicationPrewrite(ctx, run.ID, attempt.ClaimID); err != nil || released {
		t.Fatalf("published claim released=%v err=%v", released, err)
	}
	if _, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now()); err != nil || claimed {
		t.Fatalf("published claim=%v err=%v", claimed, err)
	}
}

func TestPublicationCancelledClaimCreatesNoAuthority(t *testing.T) {
	s, run := publicationClaimStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, claimed, err := s.ClaimReviewPublication(ctx, run, time.Now()); claimed || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled claim=%v err=%v", claimed, err)
	}
	if _, found, err := s.GetReviewPublicationAttempt(context.Background(), run.ID); err != nil || found {
		t.Fatalf("cancelled attempt found=%v err=%v", found, err)
	}
}
