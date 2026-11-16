package systeminstall

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ExternalEffects supplies vendor-release and package-manager observations.
// Explicit construction never substitutes an HTTP client or command adapter.
type ExternalEffects struct {
	ManagedVersion   func(context.Context, Plan) (string, error)
	OfficialVersion  func(context.Context, Target) (string, error)
	OwnsInstallation func(context.Context, string, string, string, bool) (bool, error)
}

// NewWithExternalEffects keeps the durable installer/recovery implementation
// while requiring every external operation boundary before allocating workers.
func NewWithExternalEffects(executables ports.ExecutableFinder, commands ports.CommandRunner, deps Deps, effects ExternalEffects) (*Service, error) {
	if executables == nil || commands == nil || effects.ManagedVersion == nil || effects.OfficialVersion == nil || effects.OwnsInstallation == nil {
		return nil, errors.New("installer requires explicit host and release effects")
	}
	if _, ok := commands.(ports.InstallCommandRunner); !ok {
		return nil, errors.New("installer command effect required")
	}
	if _, ok := commands.(ports.InstallScriptRunner); !ok {
		return nil, errors.New("installer script effect required")
	}
	if _, ok := commands.(ports.ManagedBinaryInstaller); !ok {
		return nil, errors.New("managed binary effect required")
	}
	if _, ok := executables.(ports.InstallCapabilityProbe); !ok {
		return nil, errors.New("installer capability effect required")
	}
	if _, ok := executables.(ports.PathWritableProbe); !ok {
		return nil, errors.New("installer writable-path effect required")
	}
	return newWithExternalEffects(executables, commands, deps, effects), nil
}
