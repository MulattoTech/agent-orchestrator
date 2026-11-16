package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var ErrPRCompletionPending = errors.New("PR_COMPLETION_PENDING")

// This is a local durable observation freshness bound, not a provider polling
// loop. A stale or missing fact waits for the ordinary SCM observer to refresh.
const prCompletionObservationMaxAge = 15 * time.Minute

type prCompletionReader interface {
	GetPR(context.Context, string) (domain.PullRequest, bool, error)
}

func (m *Manager) prCompletionDependenciesReady(ctx context.Context, dependencies []domain.PRCompletionDependency) error {
	if err := domain.ValidatePRCompletionDependencies(dependencies); err != nil {
		return err
	}
	if len(dependencies) == 0 {
		return nil
	}
	store, ok := m.store.(prCompletionReader)
	if !ok {
		return fmt.Errorf("%w: SCM observation store unavailable", ErrPRCompletionPending)
	}
	now := m.clock()
	for _, dependency := range dependencies {
		// Session status (idle, archived, or terminated) never establishes PR
		// completion. The row only proves the explicit ownership identity exists.
		_, found, err := m.store.GetSession(ctx, dependency.SessionID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: session %s missing", ErrPRCompletionPending, dependency.SessionID)
		}
		pr, found, err := store.GetPR(ctx, dependency.PRURL)
		if err != nil {
			return err
		}
		if !found || pr.URL != dependency.PRURL || pr.SessionID != dependency.SessionID || !strings.EqualFold(pr.HeadSHA, dependency.HeadSHA) {
			return fmt.Errorf("%w: exact session, PR and head observation missing", ErrPRCompletionPending)
		}
		if !pr.Merged || !domain.ValidFullCommitSHA(pr.MergeCommitSHA) || pr.MergedAtProvider.IsZero() {
			return fmt.Errorf("%w: authoritative merged result missing", ErrPRCompletionPending)
		}
		u, _ := url.Parse(dependency.PRURL) // validated above
		if pr.Provider == "" || pr.Host == "" || pr.Repo == "" || pr.ProviderID == "" || !strings.EqualFold(pr.Host, u.Host) {
			return fmt.Errorf("%w: provider observation provenance missing", ErrPRCompletionPending)
		}
		if pr.ObservedAt.IsZero() || now.Sub(pr.ObservedAt) > prCompletionObservationMaxAge || pr.ObservedAt.After(now.Add(time.Minute)) || pr.MergedAtProvider.After(pr.ObservedAt.Add(time.Minute)) {
			return fmt.Errorf("%w: merged observation stale or invalid", ErrPRCompletionPending)
		}
	}
	return nil
}
