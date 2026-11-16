package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var ErrWorkspaceScriptPolicy = errors.New("WORKSPACE_SCRIPT_POLICY_INVALID")

type workspaceScriptLimits struct{ global, project int }

// These limits count synchronously owned Setup/Cleanup shells in this daemon.
// Provider-native tools, persistent user shells and detached descendants do not
// traverse this boundary and are deliberately excluded from its claims.
func workspaceScriptLimitsFromEnv(getenv func(string) string) (workspaceScriptLimits, error) {
	limits := workspaceScriptLimits{global: 2, project: 1}
	for _, setting := range []struct {
		name string
		to   *int
	}{{"AO_MAX_WORKSPACE_SCRIPTS", &limits.global}, {"AO_MAX_PROJECT_WORKSPACE_SCRIPTS", &limits.project}} {
		value := strings.TrimSpace(getenv(setting.name))
		if value == "" {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 40 {
			return limits, fmt.Errorf("%w: %s must be an integer in 1..40", ErrWorkspaceScriptPolicy, setting.name)
		}
		*setting.to = n
	}
	if limits.project > limits.global {
		return limits, fmt.Errorf("%w: project shell limit exceeds global shell limit", ErrWorkspaceScriptPolicy)
	}
	return limits, nil
}

type workspaceScriptGate struct {
	mu      sync.Mutex
	getenv  func(string) string
	active  int
	waiting int
	project map[domain.ProjectID]int
	changed chan struct{}
}

func newWorkspaceScriptGate(getenv func(string) string) *workspaceScriptGate {
	return &workspaceScriptGate{getenv: getenv, project: make(map[domain.ProjectID]int), changed: make(chan struct{})}
}

var daemonWorkspaceScriptGate = newWorkspaceScriptGate(os.Getenv)

type workspaceScriptProjectKey struct{}
type workspaceScriptGateKey struct{}

func withWorkspaceScriptProject(ctx context.Context, projectID domain.ProjectID) context.Context {
	return context.WithValue(ctx, workspaceScriptProjectKey{}, projectID)
}

func acquireWorkspaceScript(ctx context.Context) (func(), error) {
	gate := daemonWorkspaceScriptGate
	if override, ok := ctx.Value(workspaceScriptGateKey{}).(*workspaceScriptGate); ok && override != nil {
		gate = override
	}
	project, _ := ctx.Value(workspaceScriptProjectKey{}).(domain.ProjectID)
	// Standalone/legacy callers conservatively share the empty project bucket.
	return gate.acquire(ctx, project)
}

func (g *workspaceScriptGate) acquire(ctx context.Context, project domain.ProjectID) (func(), error) {
	g.mu.Lock()
	waiting := false
	defer func() {
		if waiting {
			g.waiting--
		}
		g.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limits, err := workspaceScriptLimitsFromEnv(g.getenv)
		if err != nil {
			return nil, err
		}
		// A project at its own limit never reserves a global slot while waiting.
		if g.active < limits.global && g.project[project] < limits.project {
			g.active++
			g.project[project]++
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					defer g.mu.Unlock()
					g.active--
					g.project[project]--
					if g.project[project] == 0 {
						delete(g.project, project)
					}
					close(g.changed)
					g.changed = make(chan struct{})
				})
			}, nil
		}
		if !waiting {
			waiting = true
			g.waiting++
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
	}
}

// WorkspaceScriptAdmissionSnapshot reports owned shell invocations only.
// A shell may escape descendants; their occupancy remains unknown.
type WorkspaceScriptAdmissionSnapshot struct {
	GlobalLimit         int                      `json:"globalLimit"`
	PerProjectLimit     int                      `json:"perProjectLimit"`
	ActiveShells        int                      `json:"activeShells"`
	WaitingShells       int                      `json:"waitingShells"`
	ProjectShells       map[domain.ProjectID]int `json:"projectShells"`
	ConfigurationError  string                   `json:"configurationError,omitempty"`
	DescendantProcesses *int                     `json:"descendantProcesses"`
}

func (g *workspaceScriptGate) snapshot() WorkspaceScriptAdmissionSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	limits, err := workspaceScriptLimitsFromEnv(g.getenv)
	out := WorkspaceScriptAdmissionSnapshot{GlobalLimit: limits.global, PerProjectLimit: limits.project,
		ActiveShells: g.active, WaitingShells: g.waiting, ProjectShells: make(map[domain.ProjectID]int)}
	if err != nil {
		out.ConfigurationError = err.Error()
	}
	for id, n := range g.project {
		out.ProjectShells[id] = n
	}
	return out
}

func (m *Manager) WorkspaceScriptSnapshot() WorkspaceScriptAdmissionSnapshot {
	return daemonWorkspaceScriptGate.snapshot()
}
