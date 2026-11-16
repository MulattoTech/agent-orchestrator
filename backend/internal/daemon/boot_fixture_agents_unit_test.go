package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/codexops"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type fixtureUnitEffects struct {
	mu      sync.Mutex
	allowed map[string]int
	denied  map[string]int
}

func (e *fixtureUnitEffects) Allow(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.allowed == nil {
		e.allowed = map[string]int{}
	}
	e.allowed[name]++
}
func (e *fixtureUnitEffects) Deny(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.denied == nil {
		e.denied = map[string]int{}
	}
	e.denied[name]++
	return fmt.Errorf("fixture denied: %s", name)
}
func (e *fixtureUnitEffects) count(name string, denied bool) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if denied {
		return e.denied[name]
	}
	return e.allowed[name]
}

func TestBootFixtureRuntimeFortyOwnersPersistWithoutPhysicalPIDs(t *testing.T) {
	root := t.TempDir()
	effects := &fixtureUnitEffects{}
	refs := make([]ports.FencedRuntimeRef, 40)
	for i := range refs {
		id := domain.SessionID(fmt.Sprintf("fixture-%02d", i))
		generation := fmt.Sprintf("generation-%02d", i)
		handle, err := seedFixtureRuntimeOwner(root, id, generation, ports.FencedAlive)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ports.FencedRuntimeRef{Handle: handle, SessionID: id, Generation: generation}
	}
	// Reconstruct the adapter exactly as the next subprocess will. No Go
	// object, process handle, goroutine, or provider is shared between instances.
	for pass := 0; pass < 2; pass++ {
		runtime := fixtureRuntime(effects, root)
		for _, ref := range refs {
			got := runtime.ProbeFencedRuntime(context.Background(), ref)
			if got.Liveness != ports.FencedAlive || got.Reason != ports.FencedReasonExactMatch {
				t.Fatalf("pass%d owner%s: %#v", pass, ref.SessionID, got)
			}
			if pids, err := runtime.ProcessRootPIDs(context.Background(), ref.Handle); len(pids) != 0 || !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
				t.Fatalf("fabricated physical resource proof: %v %v", pids, err)
			}
		}
	}
	runtime := fixtureRuntime(effects, root)
	if err := runtime.Destroy(context.Background(), refs[0].Handle); err != nil {
		t.Fatal(err)
	}
	if got := fixtureRuntime(effects, root).ProbeFencedRuntime(context.Background(), refs[0]); got.Liveness != ports.FencedDead {
		t.Fatalf("synthetic cleanup did not persist: %#v", got)
	}
	if effects.count("runtime.create", true) != 0 {
		t.Fatal("observation invoked synthetic Create")
	}
}

func TestBootFixtureAmbiguousRuntimePreservesOwnerAndDirtyFile(t *testing.T) {
	root := t.TempDir()
	effects := &fixtureUnitEffects{}
	id := domain.SessionID("fixture-ambiguous")
	handle, err := seedFixtureRuntimeOwner(root, id, "old-generation", ports.FencedUnknown)
	if err != nil {
		t.Fatal(err)
	}
	path, err := fixtureRuntimeRecordPath(root, handle)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(root, "dirty.txt")
	if err := os.WriteFile(dirtyPath, []byte("fixture dirty work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := fixtureRuntime(effects, root)
	ref := ports.FencedRuntimeRef{Handle: handle, SessionID: id, Generation: "old-generation"}
	if got := runtime.ProbeFencedRuntime(context.Background(), ref); got.Liveness != ports.FencedUnknown {
		t.Fatalf("ambiguous ownership: %#v", got)
	}
	if alive, err := runtime.IsAlive(context.Background(), handle); alive || !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatalf("uncertainty became death: %v %v", alive, err)
	}
	if err := runtime.Destroy(context.Background(), handle); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatalf("ambiguous Destroy: %v", err)
	}
	if _, err := runtime.Create(context.Background(), ports.RuntimeConfig{SessionID: id, Argv: []string{"must-never-execute"}}); err == nil {
		t.Fatal("replacement Create accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("ambiguous owner changed: %v", err)
	}
	dirty, err := os.ReadFile(dirtyPath)
	if err != nil || string(dirty) != "fixture dirty work\n" {
		t.Fatal("dirty work changed")
	}
	if got := fixtureRuntime(effects, root).ProbeFencedRuntime(context.Background(), ref); got.Liveness != ports.FencedUnknown {
		t.Fatalf("reopen lost uncertainty: %#v", got)
	}
	if effects.count("runtime.create", true) != 1 {
		t.Fatal("forbidden Create was not recorded")
	}
}

func TestBootFixtureRuntimeRejectsForeignGenerationAndMalformedState(t *testing.T) {
	root := t.TempDir()
	effects := &fixtureUnitEffects{}
	handle, err := seedFixtureRuntimeOwner(root, "fixture-one", "generation-one", ports.FencedAlive)
	if err != nil {
		t.Fatal(err)
	}
	runtime := fixtureRuntime(effects, root)
	for _, ref := range []ports.FencedRuntimeRef{
		{Handle: handle, SessionID: "fixture-one", Generation: "generation-two"},
		{Handle: handle, SessionID: "fixture-two", Generation: "generation-one"},
		{Handle: handle, SessionID: "fixture-one"},
		{Handle: ports.RuntimeHandle{ID: "../foreign"}, SessionID: "fixture-one", Generation: "generation-one"},
	} {
		if got := runtime.ProbeFencedRuntime(context.Background(), ref); got.Liveness != ports.FencedUnknown {
			t.Fatalf("foreign identity admitted: %#v", got)
		}
	}
	path, err := fixtureRuntimeRecordPath(root, handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runtime.ProbeFencedRuntime(context.Background(), ports.FencedRuntimeRef{Handle: handle, SessionID: "fixture-one", Generation: "generation-one"}); got.Liveness != ports.FencedUnknown {
		t.Fatalf("partial durable state: %#v", got)
	}
	if _, err := seedFixtureRuntimeOwner(root, "fixture-one", "replacement", ports.FencedAlive); !errors.Is(err, os.ErrExist) {
		t.Fatalf("seed overwrote ambiguous state: %v", err)
	}
	if _, err := seedFixtureRuntimeOwner("relative", "id", "generation", ports.FencedAlive); err == nil {
		t.Fatal("relative fixture storage accepted")
	}
}

func TestBootFixtureAgentComponentsExerciseRealServicesWithFakes(t *testing.T) {
	t.Setenv("AO_LOCAL_LIFECYCLE_TEST", "")
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.Config{DataDir: filepath.Join(root, "data"), StateDir: filepath.Join(root, "state")}
	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	effects := &fixtureUnitEffects{}
	components, err := fixtureAgentComponents(ctx, cfg, store, codexops.NewGate(), slog.New(slog.DiscardHandler), effects)
	if err != nil {
		t.Fatal(err)
	}
	components.Service.SetSavedDefaultSignIn(components.Accounts.DefaultSignIn)
	components.Service.SetSavedDefaultLaunchEnv(components.Accounts.DefaultLaunchEnv)
	<-components.Service.WarmReadiness()
	readiness, err := components.Service.EnsureAgentReadiness(ctx, "codex", domain.AgentReadinessPurposeDisplay)
	if err != nil || readiness.Authentication.State != domain.AgentAuthenticationAuthorized {
		t.Fatalf("readiness=%#v error=%v", readiness, err)
	}
	catalog, err := components.Service.Models(ctx, "codex", "", true)
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != fixtureAgentModel || catalog.Models[0].DefaultEffort != "medium" || strings.Join(catalog.Models[0].Efforts, ",") != "medium" {
		t.Fatalf("model policy=%#v error=%v", catalog, err)
	}
	accounts, err := components.Accounts.List(ctx, true)
	if err != nil || len(accounts) != 1 || !accounts[0].SignedIn {
		t.Fatalf("synthetic account read=%#v error=%v", accounts, err)
	}
	for _, operation := range []string{"agent.registry", "agent.readiness.binary", "agent.readiness.authentication", "model.discover", "account.provider.identity", "account.provider.limits"} {
		if effects.count(operation, false) == 0 {
			t.Errorf("expected fake boundary not exercised: %s", operation)
		}
	}
	effects.mu.Lock()
	denied := len(effects.denied)
	effects.mu.Unlock()
	if denied != 0 {
		t.Fatal("service checks reached a forbidden effect")
	}
	if _, err := os.Stat(filepath.Join(components.CodexHome, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture created native credentials: %v", err)
	}
}

func TestBootFixtureAccountMutationsAndNativeLaunchAlwaysDenied(t *testing.T) {
	effects := &fixtureUnitEffects{}
	root := t.TempDir()
	clientFactory := fixtureAccountFactory{effects: effects, stateRoot: root}
	if _, err := clientFactory.Open(context.Background(), ports.CodexAccountContext{Home: filepath.Dir(root)}); err == nil {
		t.Fatal("native home outside fixture accepted")
	}
	client, err := clientFactory.Open(context.Background(), ports.CodexAccountContext{Home: filepath.Join(root, "home")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background(), true); err == nil {
		t.Fatal("credential refresh accepted")
	}
	if err := client.Logout(context.Background()); err == nil {
		t.Fatal("logout accepted")
	}
	if _, err := client.ConsumeResetCredit(context.Background(), "key"); err == nil {
		t.Fatal("reset accepted")
	}
	if _, err := client.ReadCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	agent, ok := fixtureAgentResolver(effects).Agent(domain.HarnessCodex)
	if !ok {
		t.Fatal("fake Codex missing")
	}
	if _, err := agent.GetLaunchCommand(context.Background(), ports.LaunchConfig{}); err == nil {
		t.Fatal("native launch accepted")
	}
	if _, _, err := agent.GetRestoreCommand(context.Background(), ports.RestoreConfig{}); err == nil {
		t.Fatal("native restore accepted")
	}
	if _, ok := fixtureAgentResolver(effects).Agent(domain.HarnessClaudeCode); ok {
		t.Fatal("unknown harness filled by native registry")
	}
}
