package agentaccounts

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ExplicitDeps supplies account providers and native-configuration decisions
// without selecting a native provider or reading the user's home implicitly.
// Terminals may be a rejecting implementation until the daemon late-binds its
// terminal service. A missing terminal boundary is not silently filled in.
type ExplicitDeps struct {
	Store                        ports.AgentAccountStore
	Providers                    []ports.AgentAccountProviderCLI
	Root                         string
	Terminals                    Terminals
	Logger                       *slog.Logger
	UsesCustomCodexModelProvider func() bool
}

// NewWithExplicitDeps constructs the ordinary account state machine with no
// production dependency defaults. It starts no checks and changes no files.
func NewWithExplicitDeps(deps ExplicitDeps) (*Service, error) {
	if err := ValidateExplicitDeps(deps); err != nil {
		return nil, err
	}
	return newWithProviderConfig(deps.Store, deps.Providers, deps.Root, deps.Terminals, deps.Logger, deps.UsesCustomCodexModelProvider), nil
}

// ValidateExplicitDeps validates required boundaries before provider metadata
// is read. Provider() must be an effect-free identity, as in the normal registry.
func ValidateExplicitDeps(deps ExplicitDeps) error {
	for _, dependency := range []struct {
		name  string
		value any
	}{
		{"store", deps.Store}, {"terminals", deps.Terminals}, {"logger", deps.Logger},
		{"Codex provider configuration", deps.UsesCustomCodexModelProvider},
	} {
		if explicitDependencyNil(dependency.value) {
			return fmt.Errorf("explicit account dependencies: %s is required", dependency.name)
		}
	}
	if strings.TrimSpace(deps.Root) == "" || !filepath.IsAbs(deps.Root) {
		return fmt.Errorf("explicit account dependencies: root must be absolute")
	}
	if len(deps.Providers) == 0 {
		return fmt.Errorf("explicit account dependencies: providers are required")
	}
	for _, provider := range deps.Providers {
		if explicitDependencyNil(provider) {
			return fmt.Errorf("explicit account dependencies: provider is required")
		}
	}
	seen := map[domain.AgentAccountProvider]bool{}
	for _, provider := range deps.Providers {
		id := provider.Provider()
		if (id != domain.AgentAccountProviderClaude && id != domain.AgentAccountProviderCodex) || seen[id] {
			return fmt.Errorf("explicit account dependencies: unsupported or duplicate provider %q", id)
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
