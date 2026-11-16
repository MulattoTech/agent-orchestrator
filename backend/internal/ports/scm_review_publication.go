package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrSCMReviewPublicationUncertain means a prior publication may still commit.
// Absence from a remote read is not permission to repeat the mutation.
var ErrSCMReviewPublicationUncertain = errors.New("review publication outcome is uncertain; reconciliation is read-only")

// SCMReviewPublicationNotSentError is proof from the adapter that no
// publication-capable request was attempted. Generic transport/timeout,
// cancellation and response errors after POST must never carry this type.
type SCMReviewPublicationNotSentError struct{ Cause error }

func (e *SCMReviewPublicationNotSentError) Error() string { return e.Cause.Error() }
func (e *SCMReviewPublicationNotSentError) Unwrap() error { return e.Cause }

// ReviewPublicationDidNotWrite recognizes the explicit adapter proof.
func ReviewPublicationDidNotWrite(err error) bool {
	var proof *SCMReviewPublicationNotSentError
	return errors.As(err, &proof)
}

// SCMReviewPublication carries an already-durable local verdict. The default
// is reconciliation only. AllowCreate is true only for the winner of a new
// durable publication claim, never for a recovered/uncertain attempt.
// ExistingReviewID always requires verification only, even with AllowCreate.
type SCMReviewPublication struct {
	PR               SCMPRRef
	RunID            string
	TargetSHA        string
	Verdict          domain.ReviewVerdict
	Body             string
	ExistingReviewID string
	AllowCreate      bool
}

// SCMReviewPublicationReceipt is the actual provider review state, distinct
// from the AO verdict. A COMMENTED receipt is never a GitHub approval.
type SCMReviewPublicationReceipt struct {
	ReviewID  string
	TargetSHA string
	State     string
	URL       string
	Reused    bool
}

// SCMReviewPublisher must not make a mutation unless AllowCreate is true.
type SCMReviewPublisher interface {
	PublishReview(context.Context, SCMReviewPublication) (SCMReviewPublicationReceipt, error)
}
