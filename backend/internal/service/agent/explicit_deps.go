package agent

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
)

// HarnessFactory owns every adapter instance used by initial construction and
// later readiness refreshes. An explicitly composed service never consults the
// shipped registry to fill a missing adapter.
type HarnessFactory func() []agentregistry.HarnessAgent

// NewWithExplicitDeps constructs the real service over explicit boundaries.
// Unlike NewWithDeps, no omitted model, account, native-home, or readiness
// dependency receives a production default. Validation precedes the first
// factory call; construction itself does not bootstrap accounts or start checks.
// Credential-directory ownership and permission validation remains unchanged.
func NewWithExplicitDeps(deps Deps, factory HarnessFactory) (*Service, error) {
	if err := ValidateExplicitDeps(deps, factory); err != nil {
		return nil, err
	}
	agents := factory()
	if err := validateExplicitHarnesses(agents); err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(agents))
	for _, item := range agents {
		allowed[string(item.Harness)] = struct{}{}
	}
	checkedFactory := func() []agentregistry.HarnessAgent {
		next := factory()
		if validateExplicitHarnesses(next) != nil || len(next) != len(allowed) {
			return nil
		}
		for _, item := range next {
			if _, ok := allowed[string(item.Harness)]; !ok {
				return nil
			}
		}
		return append([]agentregistry.HarnessAgent(nil), next...)
	}
	return newWithHarnessFactory(deps, append([]agentregistry.HarnessAgent(nil), agents...), checkedFactory), nil
}

// ValidateExplicitDeps is effect-free and lets a larger composition validate
// all its boundaries before invoking any constructor or adapter factory.
func ValidateExplicitDeps(deps Deps, factory HarnessFactory) error {
	for _, dependency := range []struct {
		name  string
		value any
	}{
		{"harness factory", factory}, {"context", deps.Context},
		{"logger", deps.Logger}, {"clock", deps.Clock},
		{"model cache", deps.Cache}, {"model discoverer", deps.Discoverer},
		{"session lookup", deps.Sessions}, {"model gate", deps.ModelGate},
		{"Codex account factory", deps.CodexAccounts},
		{"Codex account switch store", deps.CodexAccountSwitches},
		{"Codex operation gate", deps.CodexOperationGate},
		{"Codex provider configuration", deps.UsesCustomCodexModelProvider},
	} {
		if explicitDependencyNil(dependency.value) {
			return fmt.Errorf("explicit agent dependencies: %s is required", dependency.name)
		}
	}
	for _, root := range []struct{ name, path string }{
		{"data directory", deps.DataDir}, {"model discovery directory", deps.ModelDiscoveryDir},
		{"Codex account root", deps.CodexAccountRoot}, {"Codex pending root", deps.CodexPendingRoot},
		{"Codex switch staging root", deps.CodexSwitchStagingRoot}, {"Codex home", deps.CodexGlobalHome},
	} {
		if strings.TrimSpace(root.path) == "" || !filepath.IsAbs(root.path) {
			return fmt.Errorf("explicit agent dependencies: %s must be absolute", root.name)
		}
	}
	return deps.Context.Err()
}

func validateExplicitHarnesses(agents []agentregistry.HarnessAgent) error {
	if len(agents) == 0 {
		return fmt.Errorf("explicit agent dependencies: harness factory returned no adapters")
	}
	seen := make(map[string]bool, len(agents))
	for _, item := range agents {
		id := string(item.Harness)
		if id == "" || item.Manifest.ID != id || explicitDependencyNil(item.Agent) || seen[id] {
			return fmt.Errorf("explicit agent dependencies: missing, duplicate, or inconsistent harness %q", id)
		}
		seen[id] = true
	}
	return nil
}

func explicitDependencyNil(value any) bool {
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
