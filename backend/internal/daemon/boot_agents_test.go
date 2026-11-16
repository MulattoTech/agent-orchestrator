package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/codexops"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// Nil embedded methods are deny spies: any check, launch, account operation,
// model discovery, or terminal operation during construction panics. None of
// these fakes can reach a native binary or provider.
type bootDenyAgent struct{ ports.Agent }
type bootDenyModels struct{ ports.AgentModelDiscoverer }
type bootDenyAccounts struct {
	ports.CodexAccountClientFactory
}
type bootDenyTerminals struct{ agentaccounts.Terminals }
type bootDenyProvider struct{ ports.AgentAccountProviderCLI }

func (bootDenyProvider) Provider() domain.AgentAccountProvider {
	return domain.AgentAccountProviderCodex
}

type bootModelCache struct{ invalidations int }

func (c *bootModelCache) InvalidateBinaryResolution(domain.AgentHarness) { c.invalidations++ }

func explicitBootTestInputs(t *testing.T) (config.Config, explicitAgentBootInputs, *int) {
	t.Helper()
	root := t.TempDir()
	calls := new(int)
	return config.Config{DataDir: filepath.Join(root, "data"), StateDir: filepath.Join(root, "state")}, explicitAgentBootInputs{
		Harnesses: func() []agentregistry.HarnessAgent {
			*calls++
			return []agentregistry.HarnessAgent{{Harness: domain.HarnessCodex, Manifest: adapters.Manifest{ID: "codex", Name: "Injected"}, Agent: bootDenyAgent{}}}
		},
		Discoverer: bootDenyModels{},
		ModelGate: func(_ context.Context, _ string, catalog ports.AgentModelCatalog) ports.AgentModelCatalog {
			return catalog
		},
		CodexHome: filepath.Join(root, "codex-home"), CodexAccounts: bootDenyAccounts{},
		Providers: []ports.AgentAccountProviderCLI{bootDenyProvider{}}, AccountTerminals: bootDenyTerminals{},
		UsesCustomCodexModelProvider: func() bool { panic("unexpected configuration read during construction") },
		ModelBinaryCache:             &bootModelCache{}, Clock: time.Now,
	}, calls
}

func TestExplicitAgentBootRejectsIncompleteGraphBeforeHarnessFactory(t *testing.T) {
	cfg, base, calls := explicitBootTestInputs(t)
	ctx := context.Background()
	store := &sqlite.Store{} // No methods or files are used during construction.
	gate := codexops.NewGate()
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name string
		edit func(*explicitAgentBootInputs)
	}{
		{"registry", func(in *explicitAgentBootInputs) { in.Harnesses = nil }},
		{"model discoverer", func(in *explicitAgentBootInputs) { in.Discoverer = nil }},
		{"model gate", func(in *explicitAgentBootInputs) { in.ModelGate = nil }},
		{"native home", func(in *explicitAgentBootInputs) { in.CodexHome = "" }},
		{"account client", func(in *explicitAgentBootInputs) { in.CodexAccounts = nil }},
		{"account providers", func(in *explicitAgentBootInputs) { in.Providers = nil }},
		{"account terminals", func(in *explicitAgentBootInputs) { in.AccountTerminals = nil }},
		{"native configuration", func(in *explicitAgentBootInputs) { in.UsesCustomCodexModelProvider = nil }},
		{"model invalidator", func(in *explicitAgentBootInputs) { in.ModelBinaryCache = (*bootModelCache)(nil) }},
		{"clock", func(in *explicitAgentBootInputs) { in.Clock = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.edit(&in)
			components, err := newExplicitAgentBoot(ctx, cfg, store, gate, log, in)
			if err == nil || components.Service != nil || *calls != 0 {
				t.Fatalf("missing %s accepted or caused effect: error=%v factory calls=%d", tc.name, err, *calls)
			}
		})
	}
	for _, root := range []string{cfg.DataDir, cfg.StateDir, base.CodexHome} {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid boot wrote root %q: %v", root, err)
		}
	}
}

func TestExplicitAgentBootBuildsRealServicesWithoutNativeDefaults(t *testing.T) {
	cfg, in, calls := explicitBootTestInputs(t)
	components, err := newExplicitAgentBoot(context.Background(), cfg, &sqlite.Store{}, codexops.NewGate(), slog.New(slog.DiscardHandler), in)
	if err != nil {
		t.Fatal(err)
	}
	if components.Service == nil || components.Accounts == nil || components.CodexHome != in.CodexHome || *calls != 1 || components.validate() != nil {
		t.Fatalf("incorrect explicit components: %#v factory calls=%d", components, *calls)
	}
	if in.ModelBinaryCache.(*bootModelCache).invalidations != 0 {
		t.Fatal("constructor invalidated native model binary")
	}
	for _, root := range []string{cfg.DataDir, cfg.StateDir, in.CodexHome} {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("construction bootstrapped files before shared daemon flow: %v", err)
		}
	}
	components.ModelBinaryCache.InvalidateBinaryResolution(domain.HarnessCodex)
	if in.ModelBinaryCache.(*bootModelCache).invalidations != 1 {
		t.Fatal("binary cache injection was not retained")
	}
}

func TestAgentBootComponentsRejectIncompleteResult(t *testing.T) {
	for _, components := range []agentBootComponents{
		{},
		{Service: &agentsvc.Service{}, Accounts: &agentaccounts.Service{}},
		{Service: &agentsvc.Service{}, Accounts: &agentaccounts.Service{}, CodexHome: "relative", CodexAccounts: bootDenyAccounts{}, ModelBinaryCache: &bootModelCache{}},
		{Service: &agentsvc.Service{}, Accounts: &agentaccounts.Service{}, CodexHome: t.TempDir(), CodexAccounts: (*bootDenyAccounts)(nil), ModelBinaryCache: &bootModelCache{}},
	} {
		if components.validate() == nil {
			t.Fatal("incomplete component result accepted")
		}
	}
}
