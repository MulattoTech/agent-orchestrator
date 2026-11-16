package domain

import (
	"errors"
	"time"
)

// ErrReviewPublicationConflict prevents a durable attempt from being rebound
// to a different saved verdict or provider receipt.
var ErrReviewPublicationConflict = errors.New("review publication binding conflict")

// ReviewPublicationAttempt is the durable authority for one saved result.
// State uncertain includes an owner that has not yet reached HTTP: after a
// crash these cases are indistinguishable. Only a newly claimed attempt may
// create a review. A ready row is backed by an explicit pre-write proof.
type ReviewPublicationAttempt struct {
	RunID            string
	SessionID        SessionID
	PRURL            string
	TargetSHA        string
	Verdict          ReviewVerdict
	BodySHA256       string
	ClaimID          string
	State            string
	AttemptCount     int64
	AttemptedAt      time.Time
	ProviderReviewID string
	ProviderState    string
	ProviderURL      string
	CompletedAt      *time.Time
}
