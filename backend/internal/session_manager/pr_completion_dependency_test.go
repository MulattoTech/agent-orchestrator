package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func prCompletionFact(session domain.SessionID, now time.Time) domain.PullRequest {
	return domain.PullRequest{URL: "https://github.com/example/repo/pull/7", SessionID: session, Number: 7,
		Provider: "github", Host: "github.com", Repo: "example/repo", ProviderID: "7",
		HeadSHA: strings.Repeat("a", 40), MergeCommitSHA: strings.Repeat("b", 40), Merged: true,
		MergedAtProvider: now.Add(-time.Minute), ObservedAt: now, UpdatedAt: now}
}

func writePRCompletionFact(t *testing.T, f *admissionFixture, pr domain.PullRequest) {
	t.Helper()
	if err := f.store.WriteSCMObservation(context.Background(), pr, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionPRCompletionRefusesIncompleteFacts(t *testing.T) {
	for _, scenario := range []string{"missing_pr", "missing_session", "wrong_session", "wrong_url", "wrong_head", "not_merged", "missing_merge_sha", "invalid_merge_sha", "missing_merge_time", "missing_provider", "missing_provider_id", "missing_host", "wrong_host", "missing_repo", "missing_observation", "stale_observation", "future_observation", "future_merge"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmissionFixture(t, 4, true)
			ctx := context.Background()
			now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			f.manager.clock = func() time.Time { return now }
			owner, _, _, err := f.manager.Spawn(ctx, admissionSpawn("red"))
			if err != nil {
				t.Fatal(err)
			}
			// Neither an archived owner nor an idle controller proves completion.
			if _, err := f.manager.Kill(ctx, owner.ID); err != nil {
				t.Fatal(err)
			}
			pr := prCompletionFact(owner.ID, now)
			dependency := domain.PRCompletionDependency{SessionID: owner.ID, PRURL: pr.URL, HeadSHA: pr.HeadSHA}
			switch scenario {
			case "missing_session":
				dependency.SessionID = "missing-session"
			case "wrong_session":
				other, _, _, err := f.manager.Spawn(ctx, admissionSpawn("green"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.manager.Kill(ctx, other.ID); err != nil {
					t.Fatal(err)
				}
				dependency.SessionID = other.ID
			case "wrong_url":
				pr.URL = "https://github.com/example/repo/pull/8"
			case "wrong_head":
				pr.HeadSHA = strings.Repeat("c", 40)
			case "not_merged":
				pr.Merged = false
				pr.Closed = true
			case "missing_merge_sha":
				pr.MergeCommitSHA = ""
			case "invalid_merge_sha":
				pr.MergeCommitSHA = "accepted"
			case "missing_merge_time":
				pr.MergedAtProvider = time.Time{}
			case "missing_provider":
				pr.Provider = ""
			case "missing_provider_id":
				pr.ProviderID = ""
			case "missing_host":
				pr.Host = ""
			case "wrong_host":
				pr.Host = "different.example"
			case "missing_repo":
				pr.Repo = ""
			case "missing_observation":
				pr.ObservedAt = time.Time{}
			case "stale_observation":
				pr.ObservedAt = now.Add(-16 * time.Minute)
			case "future_observation":
				pr.ObservedAt = now.Add(2 * time.Minute)
			case "future_merge":
				pr.MergedAtProvider = now.Add(2 * time.Minute)
			}
			if scenario != "missing_pr" {
				writePRCompletionFact(t, f, pr)
			}
			cfg := admissionSpawn("blue")
			cfg.PRCompletionDependencies = []domain.PRCompletionDependency{dependency}
			_, _, _, err = f.manager.Spawn(ctx, cfg)
			var queued *SessionQueuedError
			if !errors.As(err, &queued) {
				t.Fatal("expected durable wait", err)
			}
			for range 3 {
				if err := f.manager.DispatchAdmissionQueue(ctx, 8); err != nil {
					t.Fatal(err)
				}
				requests, err := f.manager.ListAdmissionQueue(ctx)
				if err != nil || len(requests) != 1 || requests[0].State != "queued" || requests[0].Attempts != 0 || requests[0].FailureCode != "PR_COMPLETION_PENDING" || len(requests[0].PRCompletionDependencies) != 1 {
					t.Fatalf("dependency wait consumed retries or hid criteria: %+v %v", requests, err)
				}
				if live, _ := f.rt.counts(); live != 0 {
					t.Fatalf("incomplete PR fact launched %d children", live)
				}
				now = now.Add(11 * time.Second)
			}
		})
	}
}

func TestAdmissionPRCompletionFairDurableAndExactlyOnce(t *testing.T) {
	f := newAdmissionFixture(t, 4, true)
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	f.manager.clock = func() time.Time { return now }
	owner, _, _, err := f.manager.Spawn(ctx, admissionSpawn("green"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Kill(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	pr := prCompletionFact(owner.ID, now)
	pr.Merged = false
	pr.MergeCommitSHA = ""
	pr.MergedAtProvider = time.Time{}
	writePRCompletionFact(t, f, pr)
	cfg := admissionSpawn("red")
	cfg.ClientRequestID, cfg.ClientRequestHash = "pr-dependent", "immutable-pr-dependent"
	cfg.PRCompletionDependencies = []domain.PRCompletionDependency{{SessionID: owner.ID, PRURL: pr.URL, HeadSHA: pr.HeadSHA}}
	_, _, _, err = f.manager.Spawn(ctx, cfg)
	var queued *SessionQueuedError
	if !errors.As(err, &queued) {
		t.Fatal(err)
	}
	dependentID := queued.RequestID
	// Force an unrelated project into the same queue, then restore capacity.
	f.manager.SetWorkerResourceCeiling(0)
	_, _, _, err = f.manager.Spawn(ctx, admissionSpawn("blue"))
	if !errors.As(err, &queued) {
		t.Fatal(err)
	}
	readyID := queued.RequestID
	f.manager.SetWorkerResourceCeiling(4)
	if err := f.manager.DispatchAdmissionQueue(ctx, 8); err != nil {
		t.Fatal(err)
	}
	requests, err := f.manager.ListAdmissionQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if request.ID == dependentID && (request.State != "queued" || request.Attempts != 0) {
			t.Fatalf("dependent request=%+v", request)
		}
		if request.ID == readyID && request.State != "completed" {
			t.Fatalf("blocked dependency starved a ready project: %+v", request)
		}
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatalf("ready project children=%d", live)
	}
	// Both queue criteria and provider observation survive real SQLite reopen.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = sqlite.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	f.manager.clock = func() time.Time { return now }
	if err := f.store.RecoverSessionAdmission(ctx, f.manager.admission.owner, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	if err := f.manager.DispatchAdmissionQueue(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 1 {
		t.Fatal("restart forgot PR dependency", live)
	}
	pr = prCompletionFact(owner.ID, now)
	writePRCompletionFact(t, f, pr)
	now = now.Add(11 * time.Second)
	if err := f.manager.DispatchAdmissionQueue(ctx, 8); err != nil {
		t.Fatal(err)
	}
	requests, err = f.manager.ListAdmissionQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var completed domain.SessionID
	for _, request := range requests {
		if request.ID == dependentID {
			if request.State != "completed" || request.Attempts != 0 || request.SessionID == "" {
				t.Fatalf("merged proof failed to release dependency: %+v", request)
			}
			completed = request.SessionID
		}
	}
	replayed, _, _, err := f.manager.Spawn(ctx, cfg)
	if err != nil || completed == "" || replayed.ID != completed {
		t.Fatalf("idempotent result lost after PR completion: %s %v", replayed.ID, err)
	}
	if err := f.manager.DispatchAdmissionQueue(ctx, 64); err != nil {
		t.Fatal(err)
	}
	if live, _ := f.rt.counts(); live != 2 {
		t.Fatalf("PR completion dispatched more than once: children=%d", live)
	}
	t.Log("PR_COMPLETION durable exact merged observation released one dependent; ready project was not starved; provider_calls=0")
}

func TestAdmissionPRCompletionCannotBypassQueue(t *testing.T) {
	f := newAdmissionFixture(t, 4, false)
	cfg := admissionSpawn("red")
	cfg.PRCompletionDependencies = []domain.PRCompletionDependency{{SessionID: "red-1", PRURL: "https://github.com/example/repo/pull/7", HeadSHA: strings.Repeat("a", 40)}}
	if _, _, _, err := f.manager.Spawn(context.Background(), cfg); !errors.Is(err, ErrPRCompletionPending) {
		t.Fatalf("queue-disabled dependency bypassed: %v", err)
	}
	internal := context.WithValue(context.Background(), admissionQueueContextKey{}, "existing-durable-command")
	if _, _, _, err := f.manager.Spawn(internal, cfg); !errors.Is(err, ErrPRCompletionPending) {
		t.Fatalf("internal queued command bypassed the manager dependency boundary: %v", err)
	}
	cfg.PRCompletionDependencies[0].HeadSHA = "short"
	if _, _, _, err := f.manager.Spawn(context.Background(), cfg); err == nil {
		t.Fatal("invalid dependency accepted")
	}
	if live, _ := f.rt.counts(); live != 0 {
		t.Fatalf("refused dependency started %d children", live)
	}
}
