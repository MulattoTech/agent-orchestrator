package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// PublicationResult explicitly separates the saved AO verdict from GitHub's
// actual review state. Publishing a COMMENT does not grant branch approval.
type PublicationResult struct {
	Review            domain.ReviewRun `json:"review"`
	ProviderState     string           `json:"providerState" enum:"COMMENTED,APPROVED,CHANGES_REQUESTED"`
	ProviderReviewURL string           `json:"providerReviewUrl"`
	Reused            bool             `json:"reused"`
}

type publicationStore interface {
	RecordReviewPublication(context.Context, string, string, string) (bool, error)
}

type publicationLock struct {
	mu   sync.Mutex
	refs int
}

// WithReviewPublisher enables explicit, non-inference publication replay.
func WithReviewPublisher(p ports.SCMReviewPublisher) Option {
	return func(s *Service) { s.publisher = p }
}

func (s *Service) lockPublication(id string) func() {
	s.publicationMu.Lock()
	if s.publicationLocks == nil {
		s.publicationLocks = make(map[string]*publicationLock)
	}
	entry := s.publicationLocks[id]
	if entry == nil {
		entry = &publicationLock{}
		s.publicationLocks[id] = entry
	}
	entry.refs++
	s.publicationMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.publicationMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.publicationLocks, id)
		}
		s.publicationMu.Unlock()
	}
}

// Publish reuses a saved result. A failed/missing verdict cannot be guessed,
// and stale PR heads are rejected by the provider immediately before POST.
// The stable run marker lets a replay recover a receipt after a daemon crash.
func (s *Service) Publish(ctx context.Context, workerID domain.SessionID, runID string) (PublicationResult, error) {
	if workerID == "" || strings.TrimSpace(runID) == "" {
		return PublicationResult{}, fmt.Errorf("%w: worker and run are required", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	unlock := s.lockPublication(runID)
	defer unlock()
	if s.store == nil {
		return PublicationResult{}, fmt.Errorf("review store is unavailable")
	}
	if _, ok := s.store.(publicationStore); !ok {
		return PublicationResult{}, fmt.Errorf("review publication store is unavailable")
	}
	run, found, err := s.store.GetReviewRun(ctx, runID)
	if err != nil {
		return PublicationResult{}, err
	}
	if !found {
		return PublicationResult{}, fmt.Errorf("%w: review run", ErrNotFound)
	}
	if run.SessionID != workerID {
		return PublicationResult{}, fmt.Errorf("%w: review does not belong to worker", ErrInvalid)
	}
	if (run.Status != domain.ReviewRunComplete && run.Status != domain.ReviewRunDelivered) || !run.Verdict.Valid() || strings.TrimSpace(run.Body) == "" {
		return PublicationResult{}, fmt.Errorf("%w: publication needs a saved, complete verdict and body; no reviewer will be started", ErrInvalid)
	}
	receipt, err := s.publishSaved(ctx, run, run.GithubReviewID)
	if err != nil {
		return PublicationResult{}, err
	}
	if err := s.recordPublication(ctx, &run, receipt); err != nil {
		return PublicationResult{}, err
	}
	return PublicationResult{Review: run, ProviderState: receipt.State, ProviderReviewURL: receipt.URL, Reused: receipt.Reused}, nil
}

func (s *Service) publishSaved(ctx context.Context, run domain.ReviewRun, existingID string) (ports.SCMReviewPublicationReceipt, error) {
	if s.publisher == nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review publisher is unavailable", ErrInvalid)
	}
	prs, err := s.store.ListPRsBySession(ctx, run.SessionID)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	pr, found := selectRereviewPR(prs, run.PRURL)
	if !found {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review PR is not tracked by the worker", ErrNotFound)
	}
	ref, err := reviewRequestRef(pr)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	receipt, err := s.publisher.PublishReview(ctx, ports.SCMReviewPublication{PR: ref, RunID: run.ID, TargetSHA: run.TargetSHA, Verdict: run.Verdict, Body: run.Body, ExistingReviewID: existingID})
	if errors.Is(err, ports.ErrSCMHeadChanged) {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: reviewed commit is no longer the PR head; saved verdict remains intact", ErrConflict)
	}
	if errors.Is(err, ports.ErrSCMRejected) || errors.Is(err, ports.ErrSCMUnsupported) {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if errors.Is(err, ports.ErrSCMNotFound) {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	if receipt.ReviewID == "" || receipt.TargetSHA != run.TargetSHA {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: invalid publication receipt", ErrInvalid)
	}
	switch receipt.State {
	case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
	default:
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: unsubmitted publication receipt", ErrInvalid)
	}
	return receipt, nil
}

func (s *Service) recordPublication(ctx context.Context, run *domain.ReviewRun, receipt ports.SCMReviewPublicationReceipt) error {
	store, ok := s.store.(publicationStore)
	if !ok {
		return fmt.Errorf("review publication store is unavailable")
	}
	updated, err := store.RecordReviewPublication(ctx, run.ID, run.TargetSHA, receipt.ReviewID)
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("%w: publication receipt conflicts with the saved result", ErrConflict)
	}
	run.GithubReviewID = receipt.ReviewID
	if s.reviewRefresher != nil {
		s.reviewRefresher.RequestReviewRefresh(run.PRURL)
	}
	return nil
}
