package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/sessionsearch"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestExplicitHostObserversRejectMissingBeforeEffects(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A deliberately unopened store must never be touched on invalid inputs.
	store := &sqlite.Store{}
	if _, err := startSessionSearchWithRoots(context.Background(), store, sessionsearch.TranscriptRoots{}, logger); err == nil {
		t.Fatal("search accepted unresolved roots")
	}
	if _, err := newUsagePricingRuntimeWithFetcher(usagePricingRuntimeConfig{DataDir: t.TempDir(), Store: store, Logger: logger}); err == nil {
		t.Fatal("pricing accepted implicit network fetcher")
	}
	if _, err := startReliabilityWithObservations(context.Background(), config.Config{}, store, nil, nil, "", logger, reliabilityObservations{}); err == nil {
		t.Fatal("reliability accepted implicit host/Git observation")
	}
}
