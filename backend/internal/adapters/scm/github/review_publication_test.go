package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const publicationSHA = "1111111111111111111111111111111111111111"

type publicationHTTPFake struct {
	mu                sync.Mutex
	head              string
	headReads         int
	moveOnSecondRead  bool
	reviews           []publishedReview
	posts             int
	loseFirstResponse bool
	request           ports.SCMReviewPublication
}

func publicationRequest() ports.SCMReviewPublication {
	return ports.SCMReviewPublication{PR: ports.SCMPRRef{Repo: ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "r"}, Number: 1}, RunID: "run-1", TargetSHA: publicationSHA, Verdict: domain.VerdictApproved, Body: "Saved verdict with apostrophe: it's intact."}
}
func publicationProvider(t *testing.T, f *publicationHTTPFake) *Provider {
	t.Helper()
	f.head = publicationSHA
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"offline-reviewer","type":"User"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			f.headReads++
			if f.moveOnSecondRead && f.headReads > 1 {
				f.head = strings.Repeat("2", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "open", "head": map[string]string{"sha": f.head}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1/reviews":
			_ = json.NewEncoder(w).Encode(f.reviews)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1/reviews/991":
			if len(f.reviews) == 0 {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.reviews[0])
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/1/reviews":
			var payload struct {
				CommitID string `json:"commit_id"`
				Event    string `json:"event"`
				Body     string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				http.Error(w, "bad JSON", 400)
				return
			}
			if payload.CommitID != publicationSHA || payload.Event != "COMMENT" || payload.Body != publicationBody(publicationRequest()) {
				t.Errorf("unbound or altered publication payload: %+v", payload)
			}
			f.posts++
			review := publishedReview{ID: 991, CommitID: payload.CommitID, State: "COMMENTED", Body: payload.Body, HTMLURL: "https://github.com/o/r/pull/1#pullrequestreview-991"}
			review.User.Login = "offline-reviewer"
			f.reviews = append(f.reviews, review)
			if f.loseFirstResponse {
				f.loseFirstResponse = false
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(review)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 500)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewProvider(ProviderOptions{Client: NewClient(ClientOptions{HTTPClient: server.Client(), RESTBase: server.URL})})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
func TestSavedReviewPublicationReconcilesLostResponseWithoutDuplicate(t *testing.T) {
	fake := &publicationHTTPFake{loseFirstResponse: true, reviews: []publishedReview{}}
	provider := publicationProvider(t, fake)
	request := publicationRequest()
	if _, err := provider.PublishReview(context.Background(), request); err == nil {
		t.Fatal("expected lost response")
	}
	receipt, err := provider.PublishReview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fake.posts != 1 || !receipt.Reused || receipt.ReviewID != "991" || receipt.State != "COMMENTED" || receipt.TargetSHA != publicationSHA {
		t.Fatalf("receipt=%+v posts=%d", receipt, fake.posts)
	}
	request.ExistingReviewID = receipt.ReviewID
	receipt, err = provider.PublishReview(context.Background(), request)
	if err != nil || !receipt.Reused || fake.posts != 1 {
		t.Fatalf("known receipt=%+v error=%v posts=%d", receipt, err, fake.posts)
	}
}
func TestSavedReviewPublicationRejectsHeadMovementBeforePost(t *testing.T) {
	fake := &publicationHTTPFake{moveOnSecondRead: true, reviews: []publishedReview{}}
	provider := publicationProvider(t, fake)
	if _, err := provider.PublishReview(context.Background(), publicationRequest()); !errors.Is(err, ports.ErrSCMHeadChanged) {
		t.Fatalf("error=%v", err)
	}
	if fake.posts != 0 {
		t.Fatal("posted after head advanced")
	}
}
func TestSavedReviewPublicationDoesNotTrustForeignOrWrongCommitReceipt(t *testing.T) {
	for _, kind := range []string{"foreign marker", "wrong commit", "missing known receipt", "pending receipt"} {
		t.Run(kind, func(t *testing.T) {
			fake := &publicationHTTPFake{reviews: []publishedReview{}}
			request := publicationRequest()
			review := publishedReview{ID: 991, CommitID: publicationSHA, State: "COMMENTED", Body: publicationBody(request)}
			review.User.Login = "offline-reviewer"
			switch kind {
			case "foreign marker":
				review.User.Login = "another-user"
			case "wrong commit":
				review.CommitID = strings.Repeat("3", 40)
				request.ExistingReviewID = "991"
			case "missing known receipt":
				request.ExistingReviewID = "991"
			case "pending receipt":
				review.State = "PENDING"
				request.ExistingReviewID = "991"
			}
			if kind != "missing known receipt" {
				fake.reviews = append(fake.reviews, review)
			}
			provider := publicationProvider(t, fake)
			if _, err := provider.PublishReview(context.Background(), request); err == nil {
				t.Fatal("invalid receipt accepted")
			}
			if fake.posts != 0 {
				t.Fatal("receipt verification created a review")
			}
		})
	}
}
