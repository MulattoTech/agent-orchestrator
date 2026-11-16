package daemon

import (
	"context"
	"errors"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"strings"
	"testing"
	"time"
)

type fakeReliabilityPRReader struct {
	facts map[domain.SessionID][]domain.PRFacts
	prs   map[string]domain.PullRequest
	err   error
}

func (f fakeReliabilityPRReader) ListPRFactsForSessions(context.Context, []domain.SessionID) (map[domain.SessionID][]domain.PRFacts, error) {
	return f.facts, f.err
}
func (f fakeReliabilityPRReader) GetPR(_ context.Context, url string) (domain.PullRequest, bool, error) {
	p, ok := f.prs[url]
	return p, ok, nil
}
func TestReliabilityUsageSeparatesModelEvidenceFromMergedOutcome(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	merged := now.Add(10 * time.Minute)
	rows := []inferenceguard.SessionUsageRow{{Session: "s", Model: "astra", FirstDispatchAt: &now}, {Session: "s", Model: "sol", FirstDispatchAt: &later}, {Session: "unbound", Model: "luna"}}
	p := domain.PullRequest{URL: "https://example.test/o/r/pull/1", SessionID: "s", Merged: true, HeadSHA: strings.Repeat("a", 40), MergeCommitSHA: strings.Repeat("b", 40), MergedAtProvider: merged, ObservedAt: merged.Add(time.Second), Provider: "github", ProviderID: "1"}
	f := fakeReliabilityPRReader{facts: map[domain.SessionID][]domain.PRFacts{"s": {{URL: p.URL, Merged: true}, {URL: p.URL, Merged: true}, {URL: "https://example.test/o/r/pull/2", Merged: true}}}, prs: map[string]domain.PullRequest{p.URL: p}}
	out, e := reliabilityUsageOutcomes(context.Background(), f, rows)
	if e != nil || len(out) != 2 {
		t.Fatal(out, e)
	}
	s := out[0]
	if s.TrackedPRs != 2 || s.VerifiedMergedPRs == nil || *s.VerifiedMergedPRs != 1 || s.FirstTrackedMergeLatencyMS == nil || *s.FirstTrackedMergeLatencyMS != 600000 {
		t.Fatal(s)
	}
	if s.AcceptedRequirements != nil || s.ProtectedMergeVerified != nil || s.ProductiveActionLatencyMS != nil {
		t.Fatal("inferred acceptance", s)
	}
	if out[1].FirstTrackedMergeLatencyMS != nil {
		t.Fatal("inferred historical latency")
	}
	for _, mutate := range []func(*domain.PullRequest){
		func(p *domain.PullRequest) { p.SessionID = "other" }, func(p *domain.PullRequest) { p.ProviderID = "" }, func(p *domain.PullRequest) { p.MergeCommitSHA = "" }, func(p *domain.PullRequest) { p.MergedAtProvider = p.ObservedAt.Add(time.Hour) },
	} {
		bad := p
		mutate(&bad)
		f.prs[p.URL] = bad
		out, e = reliabilityUsageOutcomes(context.Background(), f, rows)
		if e != nil || *out[0].VerifiedMergedPRs != 0 || out[0].FirstTrackedMergeLatencyMS != nil {
			t.Fatal(out, e)
		}
	}
	f.err = errors.New("unavailable")
	if _, e = reliabilityUsageOutcomes(context.Background(), f, rows); e == nil {
		t.Fatal("unknown outcomes reported as zero")
	}
}
