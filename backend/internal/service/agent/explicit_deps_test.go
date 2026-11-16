package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/codexops"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Any unplanned store access panics through the nil embedded interface;
// construction and readiness below must not bootstrap account mutations.
type explicitSwitchStore struct{ ports.CodexAccountSwitchStore }

func explicitTestDeps(t *testing.T) Deps {
	t.Helper()
	root := t.TempDir()
	return Deps{
		DataDir: root, ModelDiscoveryDir: filepath.Join(root, "models"),
		Cache: &fakeModelCache{}, Discoverer: successfulModelDiscoverer(),
		Sessions: fakeSessionUsageLookup{}, Context: context.Background(),
		Logger: slog.New(slog.DiscardHandler), Clock: time.Now,
		CodexAccountRoot: filepath.Join(root, "accounts"), CodexPendingRoot: filepath.Join(root, "pending"),
		CodexSwitchStagingRoot: filepath.Join(root, "staging"), CodexGlobalHome: filepath.Join(root, "codex-home"),
		CodexAccounts: &fakeCodexAccountFactory{}, CodexAccountSwitches: explicitSwitchStore{},
		CodexOperationGate: codexops.NewGate(),
		ModelGate: func(_ context.Context, _ string, catalog ports.AgentModelCatalog) ports.AgentModelCatalog {
			return catalog
		},
		UsesCustomCodexModelProvider: func() bool { return true },
	}
}

func explicitTestHarness(agent ports.Agent) []agentregistry.HarnessAgent {
	return []agentregistry.HarnessAgent{{Harness: domain.HarnessCodex, Manifest: adapters.Manifest{ID: "codex", Name: "Injected Codex"}, Agent: agent}}
}

func TestExplicitAgentDependenciesRejectMissingBeforeFactory(t *testing.T) {
	base := explicitTestDeps(t)
	cases := []struct {
		name string
		edit func(*Deps)
	}{
		{"context", func(d *Deps) { d.Context = nil }},
		{"logger", func(d *Deps) { d.Logger = nil }},
		{"clock", func(d *Deps) { d.Clock = nil }},
		{"cache", func(d *Deps) { d.Cache = nil }},
		{"typed nil discoverer", func(d *Deps) { d.Discoverer = (*fakeModelDiscoverer)(nil) }},
		{"sessions", func(d *Deps) { d.Sessions = nil }},
		{"model gate", func(d *Deps) { d.ModelGate = nil }},
		{"account factory", func(d *Deps) { d.CodexAccounts = nil }},
		{"switch store", func(d *Deps) { d.CodexAccountSwitches = nil }},
		{"operation gate", func(d *Deps) { d.CodexOperationGate = nil }},
		{"provider configuration", func(d *Deps) { d.UsesCustomCodexModelProvider = nil }},
		{"data root", func(d *Deps) { d.DataDir = "" }},
		{"discovery root", func(d *Deps) { d.ModelDiscoveryDir = "relative" }},
		{"account root", func(d *Deps) { d.CodexAccountRoot = "" }},
		{"pending root", func(d *Deps) { d.CodexPendingRoot = "" }},
		{"switch root", func(d *Deps) { d.CodexSwitchStagingRoot = "" }},
		{"native home", func(d *Deps) { d.CodexGlobalHome = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := base
			tc.edit(&deps)
			calls := 0
			service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent { calls++; return explicitTestHarness(fakeAgent{}) })
			if err == nil || service != nil || calls != 0 {
				t.Fatalf("missing %s: service=%v error=%v factory calls=%d", tc.name, service != nil, err, calls)
			}
		})
	}
	if _, err := NewWithExplicitDeps(base, nil); err == nil {
		t.Fatal("missing factory accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base.Context = ctx
	calls := 0
	if _, err := NewWithExplicitDeps(base, func() []agentregistry.HarnessAgent { calls++; return nil }); err == nil || calls != 0 {
		t.Fatalf("canceled construction error=%v calls=%d", err, calls)
	}
}

func TestExplicitAgentDependenciesRetainInjectedReadinessAndCatalog(t *testing.T) {
	t.Setenv("AO_LOCAL_LIFECYCLE_TEST", "")
	// A wrong implementation may read this harmless fixture config, never the real home.
	t.Setenv("CODEX_HOME", t.TempDir())
	deps := explicitTestDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deps.Context = ctx
	var factoryCalls, authDecisionCalls, gateCalls atomic.Int32
	deps.UsesCustomCodexModelProvider = func() bool { authDecisionCalls.Add(1); return true }
	deps.ModelGate = func(_ context.Context, _ string, c ports.AgentModelCatalog) ports.AgentModelCatalog {
		gateCalls.Add(1)
		c.Source = "injected-gate"
		return c
	}
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent {
		factoryCalls.Add(1)
		return explicitTestHarness(fakeAuthAgent{status: ports.AgentAuthStatusAuthorized})
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.WarmReadiness():
	case <-ctx.Done():
		t.Fatal("injected readiness warm-up did not finish")
	}
	first := factoryCalls.Load()
	service.InvalidateAgentInstallation("codex")
	service.InvalidateAgentAuthentication("codex")
	snapshot, err := service.EnsureAgentReadiness(ctx, "codex", domain.AgentReadinessPurposeLaunch)
	if err != nil || snapshot.Installation.State != domain.AgentInstallationInstalled || snapshot.Authentication.State != domain.AgentAuthenticationAuthorized {
		t.Fatalf("injected readiness: %#v, %v", snapshot, err)
	}
	if factoryCalls.Load() <= first || authDecisionCalls.Load() == 0 {
		t.Fatalf("refresh escaped injected factory/decision: factories=%d first=%d decisions=%d", factoryCalls.Load(), first, authDecisionCalls.Load())
	}
	catalog, err := service.Models(ctx, "codex", "", true)
	if err != nil || len(catalog.Models) != 1 || catalog.Source != "injected-gate" {
		t.Fatalf("catalog=%#v error=%v", catalog, err)
	}
	discoverer := deps.Discoverer.(*fakeModelDiscoverer)
	if discoverer.discoverCalls.Load() != 1 || gateCalls.Load() == 0 {
		t.Fatalf("injected discovery/gate calls=%d/%d", discoverer.discoverCalls.Load(), gateCalls.Load())
	}
	if discoverer.lastRequest.WorkingDir != deps.ModelDiscoveryDir {
		t.Fatalf("discovery escaped explicit directory: %q", discoverer.lastRequest.WorkingDir)
	}
	accountFactory := deps.CodexAccounts.(*fakeCodexAccountFactory)
	accountFactory.mu.Lock()
	opens, checks := accountFactory.opens, accountFactory.capabilityChecks
	accountFactory.mu.Unlock()
	if opens != 0 || checks != 0 {
		t.Fatalf("construction/readiness unexpectedly bootstrapped accounts: %d/%d", opens, checks)
	}
	if _, err := service.EnsureAgentReadiness(ctx, "claude-code", domain.AgentReadinessPurposeDisplay); err == nil {
		t.Fatal("missing explicit adapter was supplied by another registry")
	}
}

func TestExplicitAgentProviderDecisionOverridesAmbientNativeHome(t *testing.T) {
	t.Setenv("AO_LOCAL_LIFECYCLE_TEST", "")
	nativeHome := t.TempDir()
	t.Setenv("CODEX_HOME", nativeHome)
	if err := os.WriteFile(filepath.Join(nativeHome, "config.toml"), []byte("model_provider = \"fixture-external\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := explicitTestDeps(t)
	var decisions atomic.Int32
	deps.UsesCustomCodexModelProvider = func() bool { decisions.Add(1); return false }
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent { return explicitTestHarness(fakeAgent{}) })
	if err != nil {
		t.Fatal(err)
	}
	service.SetSavedDefaultSignIn(func(context.Context, domain.AgentHarness) (domain.AgentAccountIdentityCheck, bool) {
		return domain.AgentAccountIdentityCheck{SignedIn: true, LoggedIn: true}, true
	})
	got, handled := service.authenticationWithAccount(context.Background(), "codex", domain.AgentReadinessPurposeDisplay)
	if !handled || got.State != domain.AgentAuthenticationAuthorized || decisions.Load() != 1 {
		t.Fatalf("ambient provider won over injected decision: %#v handled=%v decisions=%d", got, handled, decisions.Load())
	}
}

func TestExplicitAgentFactoryRejectsInvalidAndChangedRegistries(t *testing.T) {
	deps := explicitTestDeps(t)
	for name, agents := range map[string][]agentregistry.HarnessAgent{
		"empty":     nil,
		"typed nil": explicitTestHarness((*fakeAuthAgent)(nil)),
		"duplicate": append(explicitTestHarness(fakeAgent{}), explicitTestHarness(fakeAgent{})...),
		"mismatch":  {{Harness: domain.HarnessCodex, Manifest: adapters.Manifest{ID: "claude-code"}, Agent: fakeAgent{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent { return agents }); err == nil || service != nil {
				t.Fatalf("invalid factory accepted: service=%v error=%v", service != nil, err)
			}
		})
	}
	calls := 0
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent {
		calls++
		if calls == 1 {
			return explicitTestHarness(fakeAgent{})
		}
		return []agentregistry.HarnessAgent{{Harness: domain.HarnessClaudeCode, Manifest: adapters.Manifest{ID: "claude-code"}, Agent: fakeAgent{}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := service.readiness.factory(); len(got) != 0 {
		t.Fatalf("registry change was admitted: %#v", got)
	}
	if _, ok := service.readiness.freshAgent("codex"); ok {
		t.Fatal("invalid refresh fell back to the shipped registry")
	}
	if _, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent { return nil }); err == nil || !strings.Contains(err.Error(), "no adapters") {
		t.Fatalf("empty factory error=%v", err)
	}
}

func TestExplicitAgentConstructionKeepsProductionProviderDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	service := NewWithAgents(nil)
	if service.usesCustomCodexModelProvider() {
		t.Fatal("empty fixture home is unexpectedly custom")
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider = \"fixture-external\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !service.usesCustomCodexModelProvider() {
		t.Fatal("normal constructor lost native provider configuration behavior")
	}
}
