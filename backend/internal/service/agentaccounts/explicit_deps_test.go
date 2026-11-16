package agentaccounts

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

type explicitAccountProvider struct {
	metadata   atomic.Int32
	identities atomic.Int32
	limits     atomic.Int32
	mutations  atomic.Int32
	signedIn   bool
}

func (p *explicitAccountProvider) Provider() domain.AgentAccountProvider {
	p.metadata.Add(1)
	return domain.AgentAccountProviderCodex
}
func (p *explicitAccountProvider) Identity(context.Context, string) (domain.AgentAccountIdentityCheck, error) {
	p.identities.Add(1)
	return domain.AgentAccountIdentityCheck{LoggedIn: p.signedIn, SignedIn: p.signedIn}, nil
}
func (p *explicitAccountProvider) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	p.limits.Add(1)
	return domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{ID: "fixture-window", UsedPercent: 20}}}, nil
}
func (p *explicitAccountProvider) LoginArgv(context.Context) ([]string, error) {
	p.mutations.Add(1)
	return nil, errors.New("fixture forbids login")
}
func (p *explicitAccountProvider) Logout(context.Context, string) error {
	p.mutations.Add(1)
	return errors.New("fixture forbids logout")
}
func (p *explicitAccountProvider) Prepare(string) error {
	p.mutations.Add(1)
	return errors.New("fixture forbids prepare")
}
func (p *explicitAccountProvider) LaunchEnv(string) map[string]string { p.mutations.Add(1); return nil }

type explicitAccountTerminals struct{ calls atomic.Int32 }

func (t *explicitAccountTerminals) OpenCommandTerminal(context.Context, shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	t.calls.Add(1)
	return shellterm.ShellTerminal{}, errors.New("fixture forbids terminal launch")
}
func (t *explicitAccountTerminals) CloseShellTerminal(context.Context, string) error {
	t.calls.Add(1)
	return errors.New("fixture forbids terminal close")
}

type explicitLimitLog struct {
	readings chan []domain.AgentLimitReading
}

func (l *explicitLimitLog) RecordAgentLimitReadings(_ context.Context, readings []domain.AgentLimitReading) error {
	select {
	case l.readings <- readings:
	default:
	}
	return nil
}
func (*explicitLimitLog) ListAgentLimitReadings(context.Context, time.Time) ([]domain.AgentLimitReading, error) {
	return nil, nil
}
func (*explicitLimitLog) LimitHistoryEnabled(context.Context) bool { return true }

func explicitAccountDeps(t *testing.T, provider *explicitAccountProvider) ExplicitDeps {
	t.Helper()
	return ExplicitDeps{
		Store:     &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}},
		Providers: []ports.AgentAccountProviderCLI{provider}, Root: filepath.Join(t.TempDir(), "accounts"),
		Terminals: &explicitAccountTerminals{}, Logger: slog.New(slog.DiscardHandler),
		UsesCustomCodexModelProvider: func() bool { return false },
	}
}

func TestExplicitAccountDependenciesRejectMissingBeforeProviderUse(t *testing.T) {
	provider := &explicitAccountProvider{}
	base := explicitAccountDeps(t, provider)
	for _, tc := range []struct {
		name string
		edit func(*ExplicitDeps)
	}{
		{"store", func(d *ExplicitDeps) { d.Store = nil }},
		{"typed nil store", func(d *ExplicitDeps) { d.Store = (*memStore)(nil) }},
		{"providers", func(d *ExplicitDeps) { d.Providers = nil }},
		{"typed nil provider", func(d *ExplicitDeps) { d.Providers = []ports.AgentAccountProviderCLI{(*explicitAccountProvider)(nil)} }},
		{"root", func(d *ExplicitDeps) { d.Root = "" }},
		{"relative root", func(d *ExplicitDeps) { d.Root = "relative" }},
		{"terminal boundary", func(d *ExplicitDeps) { d.Terminals = nil }},
		{"logger", func(d *ExplicitDeps) { d.Logger = nil }},
		{"provider configuration", func(d *ExplicitDeps) { d.UsesCustomCodexModelProvider = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := base
			tc.edit(&deps)
			service, err := NewWithExplicitDeps(deps)
			if err == nil || service != nil || provider.metadata.Load() != 0 || provider.identities.Load() != 0 {
				t.Fatalf("missing dependency: service=%v error=%v provider calls=%d/%d", service != nil, err, provider.metadata.Load(), provider.identities.Load())
			}
		})
	}
	if _, err := os.Stat(base.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid construction wrote root: %v", err)
	}
	duplicate := base
	duplicate.Providers = append(duplicate.Providers, provider)
	if _, err := NewWithExplicitDeps(duplicate); err == nil {
		t.Fatal("duplicate providers accepted")
	}
}

func TestExplicitAccountProviderDecisionOverridesAmbientNativeHome(t *testing.T) {
	nativeHome := t.TempDir()
	t.Setenv("CODEX_HOME", nativeHome)
	if err := os.WriteFile(filepath.Join(nativeHome, "config.toml"), []byte("model_provider = \"fixture-external\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &explicitAccountProvider{}
	deps := explicitAccountDeps(t, provider)
	var custom atomic.Bool
	var decisions atomic.Int32
	deps.UsesCustomCodexModelProvider = func() bool { decisions.Add(1); return custom.Load() }
	service, err := NewWithExplicitDeps(deps)
	if err != nil {
		t.Fatal(err)
	}
	if provider.identities.Load() != 0 || provider.limits.Load() != 0 {
		t.Fatal("constructor performed provider checks")
	}
	if _, err := service.List(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.newSessionAccount(context.Background(), domain.AgentAccountProviderCodex); !errors.Is(err, ports.ErrAgentAccountDefaultUnusable) {
		t.Fatalf("native-home config overrode explicit signed-out decision: %v", err)
	}
	custom.Store(true)
	account, err := service.newSessionAccount(context.Background(), domain.AgentAccountProviderCodex)
	if err != nil || account.ID != domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex) || decisions.Load() != 2 {
		t.Fatalf("custom injected decision: %#v error=%v decisions=%d", account, err, decisions.Load())
	}
	if provider.mutations.Load() != 0 || deps.Terminals.(*explicitAccountTerminals).calls.Load() != 0 {
		t.Fatal("account read launched or mutated provider")
	}
}

func TestExplicitAccountBackgroundPollingUsesOnlyInjectedProvider(t *testing.T) {
	provider := &explicitAccountProvider{signedIn: true}
	deps := explicitAccountDeps(t, provider)
	service, err := NewWithExplicitDeps(deps)
	if err != nil {
		t.Fatal(err)
	}
	log := &explicitLimitLog{readings: make(chan []domain.AgentLimitReading, 1)}
	service.SetLimitLog(log)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); service.KeepReadingLimits(ctx, time.Millisecond) }()
	select {
	case readings := <-log.readings:
		if len(readings) != 1 || readings[0].Provider != domain.AgentAccountProviderCodex || readings[0].WindowID != "fixture-window" {
			t.Fatalf("wrong recorded injected limits: %#v", readings)
		}
	case <-ctx.Done():
		t.Fatal("real background poller did not use injected provider")
	}
	cancel()
	<-done
	if provider.identities.Load() != 1 || provider.limits.Load() != 1 {
		t.Fatalf("fresh-cache poller calls=%d/%d", provider.identities.Load(), provider.limits.Load())
	}
	if provider.mutations.Load() != 0 || deps.Terminals.(*explicitAccountTerminals).calls.Load() != 0 {
		t.Fatal("background read launched/mutated provider")
	}
}

func TestExplicitAccountConstructionKeepsProductionProviderDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	deps := explicitAccountDeps(t, &explicitAccountProvider{})
	service := New(deps.Store, deps.Providers, deps.Root, deps.Terminals, deps.Logger)
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
