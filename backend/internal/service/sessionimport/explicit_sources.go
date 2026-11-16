package sessionimport

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// NewWithSourceRoots constructs the real import service from explicit local
// roots. It never resolves the user's home or provider configuration. An empty
// nonnil roots map explicitly supplies no native import sources.
//
// Conductor's importer additionally resolves other providers' homes internally,
// so it is not admitted by this explicit constructor until those roots have
// their own dependency contract. Ordinary New retains existing behavior.
func NewWithSourceRoots(store Store, dataDir, home string, roots map[domain.AgentHarness]string, supportsChat func(domain.AgentHarness) bool) (*Service, error) {
	if store == nil || (reflect.ValueOf(store).Kind() == reflect.Pointer && reflect.ValueOf(store).IsNil()) || supportsChat == nil || roots == nil {
		return nil, errors.New("explicit import service requires store, source roots and chat capability")
	}
	if !filepath.IsAbs(dataDir) || !filepath.IsAbs(home) {
		return nil, errors.New("explicit import data and home paths must be absolute")
	}
	copied := make(map[domain.AgentHarness]string, len(roots))
	for harness, root := range roots {
		if harness == conductorSource {
			return nil, errors.New("explicit import sources do not include Conductor's implicit cross-provider homes")
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("explicit import root for %s must be absolute", harness)
		}
		importer, known := harnessImporters[harness]
		if !known {
			return nil, fmt.Errorf("unknown explicit import source %s", harness)
		}
		if importer.discoverRegistered != nil || importer.readWithProjects != nil {
			return nil, fmt.Errorf("explicit import source %s requires a separately supplied registered-project boundary", harness)
		}
		copied[harness] = canonical(root)
	}
	return &Service{store: store, supportsChat: supportsChat, home: canonical(home), roots: copied, explicitRoots: true, excluded: []string{canonical(dataDir)}, now: time.Now}, nil
}
func (s *Service) checkExplicitSource(harness domain.AgentHarness, root string, paths ...string) error {
	if !s.explicitRoots {
		return nil
	}
	configured, ok := s.roots[harness]
	if !ok || !filepath.IsAbs(root) {
		return errors.New("native import source is not explicitly configured")
	}
	relative, err := filepath.Rel(configured, canonical(root))
	if err != nil || relative != "." {
		return errors.New("native import config escaped its explicit source root")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return errors.New("native import transcript requires an explicit absolute path")
		}
		relative, err = filepath.Rel(configured, canonical(path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return errors.New("native import transcript escaped its explicit source root")
		}
	}
	return nil
}
