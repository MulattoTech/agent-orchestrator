package review_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	github "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const durablePublicationSHA = "1111111111111111111111111111111111111111"

type durablePublicationFixture struct {
	dir   string
	store *sqlite.Store
	run   domain.ReviewRun
}

func newDurablePublicationFixture(t *testing.T) *durablePublicationFixture {
	t.Helper()
	f := &durablePublicationFixture{dir: t.TempDir()}
	var err error
	f.store, err = sqlitetest.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.store.UpsertProject(ctx, domain.ProjectRecord{ID: "publication", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	session, err := f.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "publication", Kind: domain.KindWorker, Harness: domain.HarnessFake,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{WorkspacePath: t.TempDir(), Branch: "offline-publication"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{SessionID: session.ID, URL: "https://github.com/o/r/pull/1", Number: 1,
		Provider: "github", Host: "github.com", Repo: "o/r", HeadSHA: durablePublicationSHA, UpdatedAt: now}
	if err := f.store.WritePR(ctx, pr, nil, nil); err != nil {
		t.Fatal(err)
	}
	reviewer := domain.Review{ID: "publication-reviewer", SessionID: session.ID, ProjectID: session.ProjectID,
		Harness: domain.ReviewerCodex, PRURL: pr.URL, CreatedAt: now, UpdatedAt: now}
	if err := f.store.UpsertReview(ctx, reviewer); err != nil {
		t.Fatal(err)
	}
	f.run = domain.ReviewRun{ID: "publication-run", ReviewID: reviewer.ID, SessionID: session.ID, Harness: reviewer.Harness,
		PRURL: pr.URL, TargetSHA: durablePublicationSHA, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		Body: "Offline durable saved review. It remains byte-for-byte intact.", CreatedAt: now}
	if err := f.store.InsertReviewRun(ctx, f.run); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *durablePublicationFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
	var err error
	f.store, err = sqlite.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *durablePublicationFixture) assertSaved(t *testing.T, state string, count int64) domain.ReviewPublicationAttempt {
	t.Helper()
	run, ok, err := f.store.GetReviewRun(context.Background(), f.run.ID)
	if err != nil || !ok {
		t.Fatalf("run found=%v err=%v", ok, err)
	}
	if run.Body != f.run.Body || run.Verdict != f.run.Verdict || run.TargetSHA != f.run.TargetSHA || run.Status != f.run.Status {
		t.Fatalf("saved verdict changed: %+v", run)
	}
	attempt, ok, err := f.store.GetReviewPublicationAttempt(context.Background(), f.run.ID)
	if err != nil || !ok {
		t.Fatalf("attempt found=%v err=%v", ok, err)
	}
	digest := sha256.Sum256([]byte(f.run.Body))
	if attempt.RunID != f.run.ID || attempt.SessionID != f.run.SessionID || attempt.PRURL != f.run.PRURL ||
		attempt.TargetSHA != f.run.TargetSHA || attempt.Verdict != f.run.Verdict ||
		attempt.BodySHA256 != hex.EncodeToString(digest[:]) || attempt.ClaimID == "" ||
		attempt.State != state || attempt.AttemptCount != count {
		t.Fatalf("incorrect durable binding: %+v", attempt)
	}
	return attempt
}

type delayedPublicationHTTP struct {
	mu                 sync.Mutex
	posts              int
	visible            bool
	hideReceipt        bool
	loseResponse       bool
	waitForCancel      bool
	rejectIdentityOnce bool
	head               string
	closed             bool
	review             map[string]any
	posted             chan struct{}
}

func (f *delayedPublicationHTTP) provider(t *testing.T) *github.Provider {
	t.Helper()
	f.head = durablePublicationSHA
	f.posted = make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			reject := f.rejectIdentityOnce
			f.rejectIdentityOnce = false
			f.mu.Unlock()
			if reject {
				http.Error(w, "offline pre-write failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"login":"offline-reviewer","type":"User"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			state, head := "open", f.head
			if f.closed {
				state = "closed"
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"state": state, "head": map[string]string{"sha": head}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1/reviews":
			rows := []map[string]any{}
			if f.visible {
				rows = append(rows, f.review)
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1/reviews/991":
			visible, receipt := f.visible, f.review
			f.mu.Unlock()
			if !visible {
				http.Error(w, "not visible", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(receipt)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/1/reviews":
			var payload struct {
				CommitID string `json:"commit_id"`
				Event    string `json:"event"`
				Body     string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				f.mu.Unlock()
				t.Error(err)
				http.Error(w, "bad JSON", 400)
				return
			}
			if payload.CommitID != durablePublicationSHA || payload.Event != "COMMENT" || !strings.Contains(payload.Body, "<!-- ao-review-run:") {
				t.Errorf("publication is not bound to fixture result")
			}
			f.posts++
			f.review = map[string]any{"id": 991, "commit_id": payload.CommitID, "state": "COMMENTED", "body": payload.Body,
				"html_url": "https://github.com/o/r/pull/1#pullrequestreview-991", "user": map[string]string{"login": "offline-reviewer"}}
			f.visible = !f.hideReceipt
			lose, wait, receipt := f.loseResponse, f.waitForCancel, f.review
			f.loseResponse, f.waitForCancel = false, false
			f.mu.Unlock()
			select {
			case f.posted <- struct{}{}:
			default:
			}
			if wait {
				<-r.Context().Done()
				return
			}
			if lose {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(receipt)
		default:
			f.mu.Unlock()
			t.Errorf("unexpected fake HTTP request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := github.NewProvider(github.ProviderOptions{Client: github.NewClient(github.ClientOptions{HTTPClient: server.Client(), RESTBase: server.URL})})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (f *delayedPublicationHTTP) postCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.posts }

func TestDurableReviewPublicationDelayedVisibilitySurvivesSQLiteReopen(t *testing.T) {
	for _, cancelPost := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost response", true: "cancel after POST accepted"}[cancelPost], func(t *testing.T) {
			fixture := newDurablePublicationFixture(t)
			remote := &delayedPublicationHTTP{hideReceipt: true, loseResponse: !cancelPost, waitForCancel: cancelPost}
			provider := remote.provider(t)
			svc := reviewsvc.New(nil, fixture.store, reviewsvc.WithReviewPublisher(provider))
			var firstErr error
			if cancelPost {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { _, err := svc.Publish(ctx, fixture.run.SessionID, fixture.run.ID); done <- err }()
				select {
				case <-remote.posted:
					cancel()
				case <-time.After(5 * time.Second):
					cancel()
					t.Fatal("fake POST was never accepted")
				}
				firstErr = <-done
			} else {
				_, firstErr = svc.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID)
			}
			if firstErr == nil || ports.ReviewPublicationDidNotWrite(firstErr) {
				t.Fatalf("unsafe uncertain classification: %v", firstErr)
			}
			before := fixture.assertSaved(t, "uncertain", 1)
			fixture.reopen(t)
			after := fixture.assertSaved(t, "uncertain", 1)
			if before.ClaimID != after.ClaimID {
				t.Fatal("restart replaced original publication identity")
			}

			secondStore, err := sqlite.OpenPreMigrated(fixture.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer secondStore.Close()
			var wg sync.WaitGroup
			for i := 0; i < 40; i++ {
				selected := fixture.store
				if i%2 != 0 {
					selected = secondStore
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					restarted := reviewsvc.New(nil, selected, reviewsvc.WithReviewPublisher(provider))
					if _, err := restarted.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID); !errors.Is(err, ports.ErrSCMReviewPublicationUncertain) {
						t.Errorf("absent delayed receipt must remain uncertain: %v", err)
					}
				}()
			}
			wg.Wait()
			if remote.postCount() != 1 {
				t.Fatalf("uncertain retry posted %d times", remote.postCount())
			}
			fixture.assertSaved(t, "uncertain", 1)

			// The original request commits/becomes visible after the failed
			// clients and after the PR has independently closed/moved.
			remote.mu.Lock()
			remote.visible, remote.closed, remote.head = true, true, strings.Repeat("9", 40)
			remote.mu.Unlock()
			recovered := reviewsvc.New(nil, fixture.store, reviewsvc.WithReviewPublisher(provider))
			result, err := recovered.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Reused || result.ProviderState != "COMMENTED" || result.Review.GithubReviewID != "991" ||
				result.Review.TargetSHA != durablePublicationSHA || remote.postCount() != 1 {
				t.Fatalf("historical result=%+v posts=%d", result, remote.postCount())
			}
			completed := fixture.assertSaved(t, "published", 1)
			if completed.CompletedAt == nil || completed.ProviderReviewID != "991" || completed.ProviderState != "COMMENTED" {
				t.Fatalf("terminal receipt=%+v", completed)
			}
			if err := secondStore.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.reopen(t)
			again := reviewsvc.New(nil, fixture.store, reviewsvc.WithReviewPublisher(provider))
			if _, err := again.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID); err != nil {
				t.Fatal(err)
			}
			if remote.postCount() != 1 {
				t.Fatal("terminal receipt replay posted again")
			}
		})
	}
}

func TestDurableReviewPublicationFortyIndependentServicesClaimOnce(t *testing.T) {
	fixture := newDurablePublicationFixture(t)
	remote := &delayedPublicationHTTP{}
	provider := remote.provider(t)
	secondStore, err := sqlite.OpenPreMigrated(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		selected := fixture.store
		if i%2 != 0 {
			selected = secondStore
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			svc := reviewsvc.New(nil, selected, reviewsvc.WithReviewPublisher(provider))
			_, err := svc.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID)
			if err != nil && !errors.Is(err, ports.ErrSCMReviewPublicationUncertain) {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if remote.postCount() != 1 {
		t.Fatalf("posts=%d, want one", remote.postCount())
	}
	fixture.assertSaved(t, "published", 1)
}

func TestDurableReviewPublicationCrashBeforeHTTPStillReconcilesOnly(t *testing.T) {
	fixture := newDurablePublicationFixture(t)
	if _, claimed, err := fixture.store.ClaimReviewPublication(context.Background(), fixture.run, time.Now()); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	fixture.reopen(t)
	remote := &delayedPublicationHTTP{}
	provider := remote.provider(t)
	svc := reviewsvc.New(nil, fixture.store, reviewsvc.WithReviewPublisher(provider))
	if _, err := svc.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID); !errors.Is(err, ports.ErrSCMReviewPublicationUncertain) {
		t.Fatalf("error=%v", err)
	}
	if remote.postCount() != 0 {
		t.Fatal("crash recovery invented creation permission")
	}
	fixture.assertSaved(t, "uncertain", 1)
}

func TestDurableReviewPublicationProvedPrewriteAllowsExplicitRetry(t *testing.T) {
	fixture := newDurablePublicationFixture(t)
	remote := &delayedPublicationHTTP{rejectIdentityOnce: true}
	provider := remote.provider(t)
	svc := reviewsvc.New(nil, fixture.store, reviewsvc.WithReviewPublisher(provider))
	if _, err := svc.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID); !ports.ReviewPublicationDidNotWrite(err) {
		t.Fatalf("missing pre-write proof: %v", err)
	}
	first := fixture.assertSaved(t, "ready", 1)
	if remote.postCount() != 0 {
		t.Fatal("pre-write failure reached POST")
	}
	if _, err := svc.Publish(context.Background(), fixture.run.SessionID, fixture.run.ID); err != nil {
		t.Fatal(err)
	}
	second := fixture.assertSaved(t, "published", 2)
	if first.ClaimID == second.ClaimID || remote.postCount() != 1 {
		t.Fatal("explicit retry did not acquire a distinct safe claim")
	}
	if released, err := fixture.store.ReleaseReviewPublicationPrewrite(context.Background(), fixture.run.ID, first.ClaimID); err != nil || released {
		t.Fatalf("stale claim released=%v err=%v", released, err)
	}
}
