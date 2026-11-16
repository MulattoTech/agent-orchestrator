package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ClaimReviewPublication commits the immutable binding before returning the
// one-shot creation authority. Recovery never promotes an uncertain attempt.
// The first statement is a conditional write so independent Store instances
// serialize in SQLite before reading, not just behind the in-process mutex.
func (s *Store) ClaimReviewPublication(ctx context.Context, run domain.ReviewRun, now time.Time) (domain.ReviewPublicationAttempt, bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	claim := hex.EncodeToString(random[:])
	n, err := q.ClaimReviewPublicationAttempt(ctx, gen.ClaimReviewPublicationAttemptParams{
		RunID: run.ID, SessionID: string(run.SessionID), PRURL: run.PRURL,
		TargetSha: run.TargetSHA, Verdict: string(run.Verdict), Body: run.Body,
		ClaimID: claim, AttemptedAt: now,
	})
	if err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	current, err := q.GetReviewRun(ctx, run.ID)
	if err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	if !samePublicationRun(reviewRunFromRow(current), run) {
		return domain.ReviewPublicationAttempt{}, false, domain.ErrReviewPublicationConflict
	}
	row, err := q.GetReviewPublicationAttempt(ctx, run.ID)
	if err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	if !publicationAttemptMatches(row, run) {
		return domain.ReviewPublicationAttempt{}, false, domain.ErrReviewPublicationConflict
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewPublicationAttempt{}, false, err
	}
	return publicationAttemptFromRow(row), n == 1 && row.ClaimID == claim, nil
}

// ReleaseReviewPublicationPrewrite is used only for the current owner's typed
// proof that it never attempted POST. No timeout or age-based release exists.
func (s *Store) ReleaseReviewPublicationPrewrite(ctx context.Context, runID, claimID string) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	n, err := s.qw.ReleaseReviewPublicationPrewrite(ctx, gen.ReleaseReviewPublicationPrewriteParams{RunID: runID, ClaimID: claimID})
	return n == 1, err
}

// CompleteReviewPublication records the terminal receipt and review-run link
// in one transaction. A crash cannot publish a receipt for another body/head,
// or leave the two local receipt records disagreeing.
func (s *Store) CompleteReviewPublication(ctx context.Context, run domain.ReviewRun, receipt ports.SCMReviewPublicationReceipt, now time.Time) (bool, error) {
	if receipt.ReviewID == "" || receipt.TargetSHA != run.TargetSHA {
		return false, domain.ErrReviewPublicationConflict
	}
	switch receipt.State {
	case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
	default:
		return false, domain.ErrReviewPublicationConflict
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)
	// Acquire the database writer before reading the shared attempt.
	n, err := q.RecordReviewPublication(ctx, gen.RecordReviewPublicationParams{ID: run.ID, TargetSha: run.TargetSHA, GithubReviewID: receipt.ReviewID})
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, domain.ErrReviewPublicationConflict
	}
	current, err := q.GetReviewRun(ctx, run.ID)
	if err != nil {
		return false, err
	}
	row, err := q.GetReviewPublicationAttempt(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if !samePublicationRun(reviewRunFromRow(current), run) || !publicationAttemptMatches(row, run) {
		return false, domain.ErrReviewPublicationConflict
	}
	n, err = q.CompleteReviewPublicationAttempt(ctx, gen.CompleteReviewPublicationAttemptParams{
		RunID: run.ID, ClaimID: row.ClaimID, ProviderReviewID: receipt.ReviewID,
		ProviderState: receipt.State, ProviderURL: receipt.URL,
		CompletedAt: sql.NullTime{Time: now, Valid: true},
	})
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, domain.ErrReviewPublicationConflict
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// GetReviewPublicationAttempt exposes bounded attempt metadata without a
// second copy of the private body; its digest identifies the exact binding.
func (s *Store) GetReviewPublicationAttempt(ctx context.Context, runID string) (domain.ReviewPublicationAttempt, bool, error) {
	row, err := s.qr.GetReviewPublicationAttempt(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReviewPublicationAttempt{}, false, nil
	}
	if err != nil {
		return domain.ReviewPublicationAttempt{}, false, fmt.Errorf("get review publication attempt: %w", err)
	}
	return publicationAttemptFromRow(row), true, nil
}

func samePublicationRun(current, wanted domain.ReviewRun) bool {
	return current.ID == wanted.ID && current.SessionID == wanted.SessionID &&
		current.PRURL == wanted.PRURL && current.TargetSHA == wanted.TargetSHA &&
		current.Verdict == wanted.Verdict && current.Body == wanted.Body &&
		current.Verdict.Valid() && current.Body != "" &&
		(current.Status == domain.ReviewRunComplete || current.Status == domain.ReviewRunDelivered)
}

func publicationAttemptMatches(row gen.ReviewPublicationAttempt, run domain.ReviewRun) bool {
	return row.RunID == run.ID && row.SessionID == string(run.SessionID) &&
		row.PRURL == run.PRURL && row.TargetSha == run.TargetSHA &&
		row.Verdict == string(run.Verdict) && row.Body == run.Body
}

func publicationAttemptFromRow(row gen.ReviewPublicationAttempt) domain.ReviewPublicationAttempt {
	digest := sha256.Sum256([]byte(row.Body))
	out := domain.ReviewPublicationAttempt{
		RunID: row.RunID, SessionID: domain.SessionID(row.SessionID), PRURL: row.PRURL,
		TargetSHA: row.TargetSha, Verdict: domain.ReviewVerdict(row.Verdict),
		BodySHA256: hex.EncodeToString(digest[:]), ClaimID: row.ClaimID, State: row.State,
		AttemptCount: row.AttemptCount, AttemptedAt: row.AttemptedAt,
		ProviderReviewID: row.ProviderReviewID, ProviderState: row.ProviderState, ProviderURL: row.ProviderURL,
	}
	if row.CompletedAt.Valid {
		out.CompletedAt = &row.CompletedAt.Time
	}
	return out
}
