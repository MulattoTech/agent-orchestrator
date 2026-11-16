package review

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type publicationFakeStore struct {
	*fakeStore
	failReceipt bool
	receipts    int
	attempt     domain.ReviewPublicationAttempt
}

func (s *publicationFakeStore) RecordReviewPublication(_ context.Context, id, sha, receipt string) (bool, error) {
	if s.failReceipt {
		s.failReceipt = false
		return false, errors.New("simulated crash before local receipt commit")
	}
	if s.run.ID != id || s.run.TargetSHA != sha || (s.run.Status != domain.ReviewRunComplete && s.run.Status != domain.ReviewRunDelivered) || (s.run.GithubReviewID != "" && s.run.GithubReviewID != receipt) {
		return false, nil
	}
	s.run.GithubReviewID = receipt
	s.receipts++
	return true, nil
}

func (s *publicationFakeStore) ClaimReviewPublication(_ context.Context, run domain.ReviewRun, now time.Time) (domain.ReviewPublicationAttempt, bool, error) {
	if s.run.ID != run.ID || s.run.Body != run.Body || s.run.TargetSHA != run.TargetSHA || s.run.Verdict != run.Verdict {
		return domain.ReviewPublicationAttempt{}, false, domain.ErrReviewPublicationConflict
	}
	claimed := s.attempt.RunID == "" || s.attempt.State == "ready"
	if claimed {
		s.attempt = domain.ReviewPublicationAttempt{RunID: run.ID, ClaimID: "claim-1", State: "uncertain", AttemptCount: s.attempt.AttemptCount + 1, AttemptedAt: now}
	}
	return s.attempt, claimed, nil
}
func (s *publicationFakeStore) ReleaseReviewPublicationPrewrite(_ context.Context, id, claim string) (bool, error) {
	if s.attempt.RunID != id || s.attempt.ClaimID != claim || s.attempt.State != "uncertain" {
		return false, nil
	}
	s.attempt.State = "ready"
	return true, nil
}
func (s *publicationFakeStore) CompleteReviewPublication(ctx context.Context, run domain.ReviewRun, receipt ports.SCMReviewPublicationReceipt, _ time.Time) (bool, error) {
	ok, err := s.RecordReviewPublication(ctx, run.ID, run.TargetSHA, receipt.ReviewID)
	if ok && err == nil {
		s.attempt.State = "published"
	}
	return ok, err
}

type fakeSavedReviewPublisher struct {
	calls  []ports.SCMReviewPublication
	posted bool
	posts  int
	err    error
}

func (p *fakeSavedReviewPublisher) PublishReview(_ context.Context, req ports.SCMReviewPublication) (ports.SCMReviewPublicationReceipt, error) {
	p.calls = append(p.calls, req)
	if p.err != nil {
		return ports.SCMReviewPublicationReceipt{}, p.err
	}
	reused := p.posted || req.ExistingReviewID != ""
	if !reused {
		if !req.AllowCreate {
			return ports.SCMReviewPublicationReceipt{}, ports.ErrSCMReviewPublicationUncertain
		}
		p.posts++
		p.posted = true
	}
	receiptID := "991"
	if req.ExistingReviewID != "" {
		receiptID = req.ExistingReviewID
	}
	return ports.SCMReviewPublicationReceipt{ReviewID: receiptID, TargetSHA: req.TargetSHA, State: "COMMENTED", Reused: reused}, nil
}
func publicationFixture() (*publicationFakeStore, *fakeSavedReviewPublisher, *Service) {
	const sha = "1111111111111111111111111111111111111111"
	store := &publicationFakeStore{fakeStore: &fakeStore{ok: true, run: domain.ReviewRun{ID: "run-1", SessionID: "worker-1", TargetSHA: sha, PRURL: "https://github.com/o/r/pull/1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, Body: "Saved review\n\nNo required changes."}, prs: []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: sha}}}}
	publisher := &fakeSavedReviewPublisher{}
	return store, publisher, New(nil, store, WithReviewPublisher(publisher))
}
func TestPublishSavedReviewRecoversReceiptWithoutReviewer(t *testing.T) {
	store, publisher, svc := publicationFixture()
	original := store.run
	store.failReceipt = true
	if _, err := svc.Publish(context.Background(), "worker-1", "run-1"); err == nil {
		t.Fatal("expected simulated receipt write failure")
	}
	if store.run.GithubReviewID != "" || store.run.Body != original.Body || store.run.Verdict != original.Verdict {
		t.Fatal("failure changed saved result")
	}
	// Reconstruct the service to model a daemon restart. The provider has already
	// accepted the stable run marker, so recovery must reuse its receipt.
	restarted := New(nil, store, WithReviewPublisher(publisher))
	result, err := restarted.Publish(context.Background(), "worker-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if publisher.posts != 1 || !result.Reused || result.ProviderState != "COMMENTED" || result.Review.Verdict != domain.VerdictApproved || result.Review.GithubReviewID != "991" {
		t.Fatalf("result=%+v posts=%d", result, publisher.posts)
	}
	if result.Review.Body != original.Body || result.Review.TargetSHA != original.TargetSHA {
		t.Fatal("replay rewrote result")
	}
}
func TestPublishSavedReviewRejectsMissingOrStaleVerdict(t *testing.T) {
	for _, status := range []domain.ReviewRunStatus{domain.ReviewRunRunning, domain.ReviewRunFailed, domain.ReviewRunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			store, publisher, svc := publicationFixture()
			store.run.Status = status
			if _, err := svc.Publish(context.Background(), "worker-1", "run-1"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
			if len(publisher.calls) != 0 {
				t.Fatal("incomplete review reached provider")
			}
		})
	}
	store, publisher, svc := publicationFixture()
	publisher.err = ports.ErrSCMHeadChanged
	if _, err := svc.Publish(context.Background(), "worker-1", "run-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v", err)
	}
	if store.run.GithubReviewID != "" || store.run.Verdict != domain.VerdictApproved {
		t.Fatal("stale-head rejection changed saved verdict")
	}
}
func TestPublishSavedReviewSerializesConcurrentReplay(t *testing.T) {
	_, publisher, svc := publicationFixture()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Publish(context.Background(), "worker-1", "run-1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if publisher.posts != 1 {
		t.Fatalf("posts=%d", publisher.posts)
	}
	if len(svc.publicationLocks) != 0 {
		t.Fatal("publication lock leaked")
	}
}
func TestSubmitAttachesLateVerifiedReceiptWithoutChangingVerdict(t *testing.T) {
	store, publisher, svc := publicationFixture()
	result, err := svc.Submit(context.Background(), "worker-1", "run-1", store.run.Verdict, store.run.Body, "991")
	if err != nil {
		t.Fatal(err)
	}
	if result.GithubReviewID != "991" || publisher.posts != 0 || len(publisher.calls) != 1 || publisher.calls[0].ExistingReviewID != "991" {
		t.Fatalf("receipt=%+v calls=%+v", result, publisher.calls)
	}
	if _, err := svc.Submit(context.Background(), "worker-1", "run-1", domain.VerdictChangesRequested, "different", "991"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed result accepted: %v", err)
	}
}

func TestInitialSubmitCannotTrustAnUnverifiedReceipt(t *testing.T) {
	store, publisher, svc := publicationFixture()
	store.run.Status, store.run.Verdict, store.run.Body = domain.ReviewRunRunning, domain.VerdictNone, ""
	publisher.err = ports.ErrSCMNotFound
	if _, err := svc.Submit(context.Background(), "worker-1", "run-1", domain.VerdictApproved, "durable first", "991"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified receipt accepted: %v", err)
	}
	if store.run.Status != domain.ReviewRunComplete || store.run.Verdict != domain.VerdictApproved ||
		store.run.Body != "durable first" || store.run.GithubReviewID != "" {
		t.Fatalf("local result not preserved independently: %+v", store.run)
	}
	if publisher.posts != 0 || len(publisher.calls) != 1 || publisher.calls[0].AllowCreate || publisher.calls[0].ExistingReviewID != "991" {
		t.Fatalf("submit bypassed read-only publication verification: %+v", publisher.calls)
	}
}
