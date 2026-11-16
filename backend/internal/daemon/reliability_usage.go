package daemon

import (
	"context"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

type reliabilityPRReader interface {
	ListPRFactsForSessions(context.Context, []domain.SessionID) (map[domain.SessionID][]domain.PRFacts, error)
	GetPR(context.Context, string) (domain.PullRequest, bool, error)
}
type reliabilitySessionOutcome struct {
	Session                    string     `json:"session_id"`
	TrackedPRs                 int        `json:"tracked_prs"`
	VerifiedMergedPRs          *int       `json:"verified_merged_prs"`
	FirstDispatchAt            *time.Time `json:"first_dispatch_at"`
	FirstReceiptObservedAt     *time.Time `json:"first_receipt_observed_at"`
	FirstTrackedMergeLatencyMS *int64     `json:"first_tracked_merge_latency_ms"`
	ProductiveActionLatencyMS  *int64     `json:"productive_action_latency_ms"`
	ProtectedMergeVerified     *bool      `json:"protected_merge_verified"`
	AcceptedRequirements       *int       `json:"accepted_requirements"`
	Confidence                 string     `json:"confidence"`
}

// Keep downstream observations separate from model groups. A session that used
// two models did not merge its PR twice. This is association, not causal cost attribution.
func reliabilityUsageOutcomes(ctx context.Context, store reliabilityPRReader, rows []inferenceguard.SessionUsageRow) ([]reliabilitySessionOutcome, error) {
	grouped := map[domain.SessionID]*reliabilitySessionOutcome{}
	for _, row := range rows {
		id := domain.SessionID(row.Session)
		o := grouped[id]
		if o == nil {
			o = &reliabilitySessionOutcome{Session: row.Session, Confidence: "tracked session association; causal attribution and accepted requirements UNKNOWN"}
			grouped[id] = o
		}
		if row.FirstDispatchAt != nil && (o.FirstDispatchAt == nil || row.FirstDispatchAt.Before(*o.FirstDispatchAt)) {
			v := *row.FirstDispatchAt
			o.FirstDispatchAt = &v
		}
		if row.FirstReceiptObservedAt != nil && (o.FirstReceiptObservedAt == nil || row.FirstReceiptObservedAt.Before(*o.FirstReceiptObservedAt)) {
			v := *row.FirstReceiptObservedAt
			o.FirstReceiptObservedAt = &v
		}
	}
	ids := make([]domain.SessionID, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	facts, err := store.ListPRFactsForSessions(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]reliabilitySessionOutcome, 0, len(ids))
	for _, id := range ids {
		o := grouped[id]
		unique := map[string]bool{}
		merged := 0
		for _, fact := range facts[id] {
			if fact.URL == "" || unique[fact.URL] {
				continue
			}
			unique[fact.URL] = true
			o.TrackedPRs++
			if !fact.Merged {
				continue
			}
			pr, found, e := store.GetPR(ctx, fact.URL)
			if e != nil {
				return nil, e
			}
			if !found || pr.SessionID != id || pr.URL != fact.URL || !pr.Merged || !domain.ValidFullCommitSHA(pr.HeadSHA) || !domain.ValidFullCommitSHA(pr.MergeCommitSHA) || pr.MergedAtProvider.IsZero() || pr.Provider == "" || pr.ProviderID == "" || pr.ObservedAt.IsZero() || pr.MergedAtProvider.After(pr.ObservedAt.Add(time.Minute)) {
				continue
			}
			merged++
			if o.FirstDispatchAt != nil && !pr.MergedAtProvider.Before(*o.FirstDispatchAt) {
				latency := pr.MergedAtProvider.Sub(*o.FirstDispatchAt).Milliseconds()
				if o.FirstTrackedMergeLatencyMS == nil || latency < *o.FirstTrackedMergeLatencyMS {
					o.FirstTrackedMergeLatencyMS = &latency
				}
			}
		}
		o.VerifiedMergedPRs = &merged
		result = append(result, *o)
	}
	return result, nil
}
