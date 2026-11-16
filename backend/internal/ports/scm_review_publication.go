package ports

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SCMReviewPublication carries an already-durable local verdict. Publication
// must never start a reviewer or infer a new verdict. ExistingReviewID makes
// this a verification-only request; an absent remote receipt must not POST.
type SCMReviewPublication struct {
	PR               SCMPRRef
	RunID            string
	TargetSHA        string
	Verdict          domain.ReviewVerdict
	Body             string
	ExistingReviewID string
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

// SCMReviewPublisher binds publication to the reviewed commit and recovers
// uncertain prior publication by a stable run marker before posting.
type SCMReviewPublisher interface {
	PublishReview(context.Context, SCMReviewPublication) (SCMReviewPublicationReceipt, error)
}
