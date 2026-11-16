package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.SCMReviewPublisher = (*Provider)(nil)

type publishedReview struct {
	ID       int64  `json:"id"`
	CommitID string `json:"commit_id"`
	State    string `json:"state"`
	Body     string `json:"body"`
	HTMLURL  string `json:"html_url"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

// publicationMarker uses a hash so arbitrary run identifiers cannot inject
// Markdown/HTML. The body digest detects an unexpected edit or forged marker.
func publicationMarker(request ports.SCMReviewPublication) string {
	key := sha256.Sum256([]byte(request.RunID))
	digest := sha256.Sum256([]byte(string(request.Verdict) + "\x00" + request.Body))
	return "<!-- ao-review-run:" + hex.EncodeToString(key[:]) + ":" + hex.EncodeToString(digest[:]) + " -->"
}

func publicationBody(request ports.SCMReviewPublication) string {
	return "AO verdict: **" + string(request.Verdict) + "**\n\nReviewed commit: " + request.TargetSHA + "\n\n" + request.Body + "\n\n" + publicationMarker(request)
}

// PublishReview reconciles a saved result. The default is read-only; only the
// winner of a newly committed durable claim may POST, once. A receipt for the
// reviewed historical commit remains valid after the current PR moves/closes.
func (p *Provider) PublishReview(ctx context.Context, request ports.SCMReviewPublication) (receipt ports.SCMReviewPublicationReceipt, resultErr error) {
	postAttempted := false
	defer func() {
		if resultErr == nil {
			return
		}
		if postAttempted {
			resultErr = errors.Join(ports.ErrSCMReviewPublicationUncertain, resultErr)
		} else if request.AllowCreate && request.ExistingReviewID == "" &&
			!errors.Is(resultErr, ports.ErrSCMReviewPublicationUncertain) {
			resultErr = &ports.SCMReviewPublicationNotSentError{Cause: resultErr}
		}
	}()
	if p == nil || p.client == nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: review publisher is not configured")
	}
	if request.PR.Number <= 0 || request.PR.Repo.Owner == "" || request.PR.Repo.Name == "" || strings.TrimSpace(request.RunID) == "" || !request.Verdict.Valid() || strings.TrimSpace(request.Body) == "" {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: invalid saved review publication", ports.ErrSCMRejected)
	}
	if len(request.TargetSHA) != 40 && len(request.TargetSHA) != 64 {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: full reviewed commit is required", ports.ErrSCMRejected)
	}
	if _, err := hex.DecodeString(request.TargetSHA); err != nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: invalid reviewed commit", ports.ErrSCMRejected)
	}
	path := repoPath(request.PR.Repo.Owner, request.PR.Repo.Name, "pulls", strconv.Itoa(request.PR.Number))
	var head struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if request.AllowCreate && request.ExistingReviewID == "" {
		response, err := p.client.doREST(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return ports.SCMReviewPublicationReceipt{}, err
		}
		if err := json.Unmarshal(response.Body, &head); err != nil {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode publication head: %w", err)
		}
		// Do not reject an advanced head before checking for an already
		// published historical receipt. Only a new POST needs the current head.
	}
	identity, err := p.AuthenticatedIdentity(ctx)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	if identity.Login == "" {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: missing publication identity", ports.ErrSCMRejected)
	}
	verify := func(review publishedReview, reused bool) (ports.SCMReviewPublicationReceipt, error) {
		if !strings.EqualFold(review.User.Login, identity.Login) {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: receipt belongs to another author", ports.ErrSCMRejected)
		}
		if review.Body != publicationBody(request) {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: receipt does not match saved run and body", ports.ErrSCMRejected)
		}
		return publicationReceipt(review, request.TargetSHA, reused)
	}
	if request.ExistingReviewID != "" {
		id, err := strconv.ParseInt(request.ExistingReviewID, 10, 64)
		if err != nil || id <= 0 {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: invalid review receipt", ports.ErrSCMRejected)
		}
		response, err := p.client.doREST(ctx, http.MethodGet, path+"/reviews/"+request.ExistingReviewID, nil, nil)
		if err != nil {
			return ports.SCMReviewPublicationReceipt{}, err
		}
		var review publishedReview
		if err := json.Unmarshal(response.Body, &review); err != nil {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode review receipt: %w", err)
		}
		if review.ID != id {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: wrong review receipt", ports.ErrSCMRejected)
		}
		return verify(review, true)
	}

	marker := publicationMarker(request)
	for page := 1; page <= 1000; page++ {
		response, err := p.client.doREST(ctx, http.MethodGet, path+"/reviews", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, nil)
		if err != nil {
			return ports.SCMReviewPublicationReceipt{}, err
		}
		var reviews []publishedReview
		if err := json.Unmarshal(response.Body, &reviews); err != nil {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode review history: %w", err)
		}
		for _, review := range reviews {
			if strings.Contains(review.Body, marker) {
				return verify(review, true)
			}
		}
		if len(reviews) < 100 {
			break
		}
		if page == 1000 {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review history could not be exhausted safely", ports.ErrSCMRejected)
		}
	}
	if !request.AllowCreate {
		return ports.SCMReviewPublicationReceipt{}, ports.ErrSCMReviewPublicationUncertain
	}

	response, err := p.client.doREST(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	if err := json.Unmarshal(response.Body, &head); err != nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode publication head: %w", err)
	}
	if head.Head.SHA != request.TargetSHA {
		return ports.SCMReviewPublicationReceipt{}, ports.ErrSCMHeadChanged
	}
	if head.State != "open" {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: pull request is not open", ports.ErrSCMRejected)
	}
	payload := struct {
		CommitID string `json:"commit_id"`
		Event    string `json:"event"`
		Body     string `json:"body"`
	}{CommitID: request.TargetSHA, Event: "COMMENT", Body: publicationBody(request)}
	// Everything from here onward is uncertain on error, including cancellation,
	// redirects and undecodable receipts. No absence-of-marker retry is safe.
	postAttempted = true
	response, err = p.client.doREST(ctx, http.MethodPost, path+"/reviews", nil, payload)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	var review publishedReview
	if err := json.Unmarshal(response.Body, &review); err != nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode published review: %w", err)
	}
	return verify(review, false)
}

func publicationReceipt(review publishedReview, target string, reused bool) (ports.SCMReviewPublicationReceipt, error) {
	if review.ID <= 0 || review.CommitID != target {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review receipt does not match reviewed commit", ports.ErrSCMRejected)
	}
	switch review.State {
	case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
	default:
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review is not submitted", ports.ErrSCMRejected)
	}
	return ports.SCMReviewPublicationReceipt{ReviewID: strconv.FormatInt(review.ID, 10), TargetSHA: review.CommitID, State: review.State, URL: review.HTMLURL, Reused: reused}, nil
}
