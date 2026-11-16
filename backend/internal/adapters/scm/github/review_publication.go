package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// PublishReview posts only a COMMENT review. This is deliberate: the signed-in
// account may own the PR, and an AO verdict must not masquerade as GitHub's
// protected-branch approval. No shell or model is involved.
func (p *Provider) PublishReview(ctx context.Context, request ports.SCMReviewPublication) (ports.SCMReviewPublicationReceipt, error) {
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
	response, err := p.client.doREST(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	var head struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
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
	identity, err := p.AuthenticatedIdentity(ctx)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	if identity.Login == "" {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: missing publication identity", ports.ErrSCMRejected)
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
		if !strings.EqualFold(review.User.Login, identity.Login) {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: receipt belongs to another author", ports.ErrSCMRejected)
		}
		return publicationReceipt(review, request.TargetSHA, true)
	}
	// A durable run survives a crash between the provider POST and local receipt
	// storage. Search all pages before POST so replay reuses that exact review.
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
				if !strings.EqualFold(review.User.Login, identity.Login) || review.Body != publicationBody(request) {
					return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: conflicting publication marker", ports.ErrSCMRejected)
				}
				return publicationReceipt(review, request.TargetSHA, true)
			}
		}
		if len(reviews) < 100 {
			break
		}
		if page == 1000 {
			return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("%w: review history could not be exhausted safely", ports.ErrSCMRejected)
		}
	}
	response, err = p.client.doREST(ctx, http.MethodGet, path, nil, nil)
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
	response, err = p.client.doREST(ctx, http.MethodPost, path+"/reviews", nil, payload)
	if err != nil {
		return ports.SCMReviewPublicationReceipt{}, err
	}
	var review publishedReview
	if err := json.Unmarshal(response.Body, &review); err != nil {
		return ports.SCMReviewPublicationReceipt{}, fmt.Errorf("github scm: decode published review: %w", err)
	}
	return publicationReceipt(review, request.TargetSHA, false)
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
