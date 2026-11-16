package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	claudecodeagent "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	codexagent "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/modelcatalog"
	chatdriveracp "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// agentBootComponents groups the actual services and the boundaries reused by
// account reconciliation, inference accounting, and binary invalidation.
type agentBootComponents struct {
	Service          *agentsvc.Service
	Accounts         *agentaccounts.Service
	CodexHome        string
	CodexAccounts    ports.CodexAccountClientFactory
	ModelBinaryCache harnessBinaryCache
}

func (c agentBootComponents) validate() error {
	if c.Service == nil || c.Accounts == nil || bootAgentDependencyNil(c.CodexAccounts) || bootAgentDependencyNil(c.ModelBinaryCache) {
		return fmt.Errorf("agent boot components: every service and boundary is required")
	}
	if strings.TrimSpace(c.CodexHome) == "" || !filepath.IsAbs(c.CodexHome) {
		return fmt.Errorf("agent boot components: Codex home must be absolute")
	}
	return nil
}

// productionAgentBoot preserves the shipped model discovery, native-home
// selection, adapters, and account providers. Only the production boot
// composition selects it; explicit fixture construction never calls it.
func productionAgentBoot(ctx context.Context, cfg config.Config, store *sqlite.Store, gate ports.CodexOperationGate, log *slog.Logger) (agentBootComponents, error) {
	codexModelPlugin := codexagent.New()
	codexModelDriver := codexappserver.New(codexModelPlugin, log)
	modelDiscoverer := modelcatalog.Discoverer{
		CodexModels: func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.ChatModel, error) {
			return codexModelDriver.DiscoverModels(listCtx, request.WorkingDir, request.Env)
		},
		ACPOptions: map[string]modelcatalog.ACPOptionListFunc{
			"cline": func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.ChatConfigOption, error) {
				return chatdriveracp.DiscoverConfigOptions(listCtx, chatdriveracp.Launch{
					Command: request.Binary,
					Args:    []string{"--acp"},
					Env:     request.Env,
				}, request.WorkingDir, log)
			},
			"deepseek-harness": func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.ChatConfigOption, error) {
				return chatdriveracp.DiscoverConfigOptions(listCtx, chatdriveracp.Launch{
					Command: request.Binary,
					Args:    []string{"--profile", "acp"},
					Env:     request.Env,
				}, request.WorkingDir, log)
			},
			"devin": func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.ChatConfigOption, error) {
				return chatdriveracp.DiscoverConfigOptions(listCtx, chatdriveracp.Launch{
					Command: request.Binary,
					Args:    []string{"acp"},
					Env:     request.Env,
				}, request.WorkingDir, log)
			},
			"hermes": func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.ChatConfigOption, error) {
				return chatdriveracp.DiscoverConfigOptionsForHarness(listCtx, chatdriveracp.Launch{
					Command: request.Binary,
					Args:    []string{"acp"},
					Env:     request.Env,
				}, request.WorkingDir, domain.HarnessHermes, log)
			},
		},
		// Claude's model IDs are provider-specific — first-party aliases,
		// Bedrock ARNs-in-miniature, Vertex @-versions — so the list has to come
		// from whichever provider is configured. On failure, the service retains
		// the matching persisted catalog instead of inventing model aliases.
		ClaudeModels: func(listCtx context.Context, request ports.AgentModelDiscoveryRequest) ([]ports.AgentModelInfo, error) {
			return claudecodeagent.ProviderModels(listCtx, request.Binary, request.WorkingDir, request.Env)
		},
		ClaudeFingerprint: func(fingerprintCtx context.Context, request ports.AgentModelDiscoveryRequest) string {
			return claudecodeagent.ProviderCatalogFingerprint(fingerprintCtx, request.Binary, request.WorkingDir, request.Env)
		},
	}
	codexPlugin := codexagent.New()
	codexHome, err := codexPlugin.NativeSessionConfigDir(ctx, nil)
	if err != nil {
		return agentBootComponents{}, fmt.Errorf("resolve device-global Codex home: %w", err)
	}

	agentDeps := agentsvc.Deps{
		DataDir: cfg.DataDir,
		Cache:   store, Discoverer: modelDiscoverer, Sessions: store, Context: ctx, Logger: log,
		ModelDiscoveryDir:      filepath.Join(cfg.DataDir, "model-discovery"),
		CodexAccountRoot:       filepath.Join(cfg.StateDir, "harnesses", "codex", "accounts"),
		CodexPendingRoot:       filepath.Join(cfg.StateDir, "harnesses", "codex", "pending-accounts"),
		CodexSwitchStagingRoot: filepath.Join(cfg.StateDir, "harnesses", "codex", "switch-staging"),
		CodexGlobalHome:        codexHome,
		CodexAccountSwitches:   store,
		CodexAccounts: codexappserver.NewAccountFactoryWithResolver(func(resolveCtx context.Context) (string, error) {
			return codexagent.New().ResolveBinary(resolveCtx)
		}, log),
		CodexOperationGate: gate,
		ModelGate:          claudeModelGate,
	}
	// Minimum Claude Code versions the API has reported per model persist
	// across restarts so a model stays marked unavailable until the CLI updates.
	claudecodeagent.ConfigureModelVersionRequirements(
		filepath.Join(cfg.StateDir, "harnesses", "claude-code", "model-versions.json"))
	agentSvc := agentsvc.NewWithDeps(agentDeps)

	return agentBootComponents{
		Service:          agentSvc,
		Accounts:         newAgentAccounts(cfg, store, codexHome, agentDeps.CodexAccounts, log),
		CodexHome:        codexHome,
		CodexAccounts:    agentDeps.CodexAccounts,
		ModelBinaryCache: pluginBinaryCache{harness: domain.HarnessCodex, plugin: codexModelPlugin},
	}, nil
}

// explicitAgentBootInputs contains every native boundary needed by these real
// services. This private composition has no env flag and no fallback to a
// production adapter, model discovery command, account provider, or home.
type explicitAgentBootInputs struct {
	Harnesses                    agentsvc.HarnessFactory
	Discoverer                   ports.AgentModelDiscoverer
	ModelGate                    agentsvc.ModelGate
	CodexHome                    string
	CodexAccounts                ports.CodexAccountClientFactory
	Providers                    []ports.AgentAccountProviderCLI
	AccountTerminals             agentaccounts.Terminals
	UsesCustomCodexModelProvider func() bool
	ModelBinaryCache             harnessBinaryCache
	Clock                        func() time.Time
}

// newExplicitAgentBoot validates the complete graph before invoking the
// harness factory. Real startup/reconciliation/checks are left to the shared
// daemon flow, and credential ACL guards remain enabled.
func newExplicitAgentBoot(ctx context.Context, cfg config.Config, store *sqlite.Store, gate ports.CodexOperationGate, log *slog.Logger, in explicitAgentBootInputs) (agentBootComponents, error) {
	if store == nil || bootAgentDependencyNil(in.ModelBinaryCache) {
		return agentBootComponents{}, fmt.Errorf("explicit agent boot: store and model binary cache are required")
	}
	if !filepath.IsAbs(cfg.DataDir) || !filepath.IsAbs(cfg.StateDir) {
		return agentBootComponents{}, fmt.Errorf("explicit agent boot: data and state directories must be absolute")
	}
	agentDeps := agentsvc.Deps{
		DataDir: cfg.DataDir, Cache: store, Discoverer: in.Discoverer,
		Sessions: store, Context: ctx, Logger: log, Clock: in.Clock,
		ModelDiscoveryDir:      filepath.Join(cfg.DataDir, "model-discovery"),
		CodexAccountRoot:       filepath.Join(cfg.StateDir, "harnesses", "codex", "accounts"),
		CodexPendingRoot:       filepath.Join(cfg.StateDir, "harnesses", "codex", "pending-accounts"),
		CodexSwitchStagingRoot: filepath.Join(cfg.StateDir, "harnesses", "codex", "switch-staging"),
		CodexGlobalHome:        in.CodexHome, CodexAccountSwitches: store,
		CodexAccounts: in.CodexAccounts, CodexOperationGate: gate,
		ModelGate: in.ModelGate, UsesCustomCodexModelProvider: in.UsesCustomCodexModelProvider,
	}
	accountDeps := agentaccounts.ExplicitDeps{
		Store: store, Providers: in.Providers,
		Root:      filepath.Join(cfg.StateDir, "agent-accounts"),
		Terminals: in.AccountTerminals, Logger: log,
		UsesCustomCodexModelProvider: in.UsesCustomCodexModelProvider,
	}
	if err := agentsvc.ValidateExplicitDeps(agentDeps, in.Harnesses); err != nil {
		return agentBootComponents{}, err
	}
	if err := agentaccounts.ValidateExplicitDeps(accountDeps); err != nil {
		return agentBootComponents{}, err
	}
	accounts, err := agentaccounts.NewWithExplicitDeps(accountDeps)
	if err != nil {
		return agentBootComponents{}, err
	}
	service, err := agentsvc.NewWithExplicitDeps(agentDeps, in.Harnesses)
	if err != nil {
		return agentBootComponents{}, err
	}
	result := agentBootComponents{
		Service: service, Accounts: accounts, CodexHome: in.CodexHome,
		CodexAccounts: in.CodexAccounts, ModelBinaryCache: in.ModelBinaryCache,
	}
	return result, result.validate()
}

func bootAgentDependencyNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
