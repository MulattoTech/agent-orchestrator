package controllers_test

import (
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
)

func TestReviewPublicationRouteSeparatesProviderState(t *testing.T) {
	svc := &fakeReviewService{publication: reviewsvc.PublicationResult{Review: domain.ReviewRun{ID: "saved", Verdict: domain.VerdictApproved, GithubReviewID: "991"}, ProviderState: "COMMENTED", Reused: true}}
	srv := newReviewTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/sessions/worker-1/reviews/publish", `{"runId":"saved"}`)
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got reviewsvc.PublicationResult
	mustJSON(t, body, &got)
	if got.ProviderState != "COMMENTED" || got.Review.Verdict != domain.VerdictApproved || !got.Reused || svc.publishedSession != "worker-1" || svc.publishedRun != "saved" {
		t.Fatalf("result=%+v", got)
	}
}
func TestReviewPublicationStaleHeadReturnsConflict(t *testing.T) {
	srv := newReviewTestServer(t, &fakeReviewService{publishErr: reviewsvc.ErrConflict})
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/worker-1/reviews/publish", `{"runId":"saved"}`)
	assertErrorCode(t, body, status, http.StatusConflict, "REVIEW_PUBLICATION_CONFLICT")
}
