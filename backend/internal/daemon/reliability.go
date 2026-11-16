package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/fleetpressure"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativereceipts"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type reliabilityAdmission interface {
	WorkspaceScriptSnapshot() sessionmanager.WorkspaceScriptAdmissionSnapshot
	HealthSnapshot(context.Context) (domain.SessionHealthSnapshot, error)
	AdmissionSnapshot(context.Context) (domain.SessionAdmissionSnapshot, error)
	ListAdmissionQueue(context.Context) ([]domain.SessionAdmissionRequest, error)
	CancelAdmissionRequest(context.Context, string) (bool, error)
	UpdateAdmissionLimits(context.Context, domain.SessionAdmissionLimits) error
	SetWorkerResourceCeiling(int)
	SetAdmissionDispatchCheck(func(context.Context) error)
}
type reliabilityObserver struct {
	mu            sync.Mutex
	store         *sqlite.Store
	admission     reliabilityAdmission
	gate          *inferenceguard.Gate
	receipts      *nativereceipts.Observer
	host          reliabilityHostSampler
	worktreeCount func(context.Context, *sqlite.Store) (int, error)
	pressure      *fleetpressure.Controller
	ownerCeiling  int
	autoscale     bool
	latest        fleetpressure.Decision
	merged        *int64
	errors        []string
	at            time.Time
}

type reliabilityHostSampler interface {
	Sample(context.Context, time.Time) (fleetpressure.Measurement, error)
	HandleObservation() fleetpressure.HandleObservation
}

type reliabilityObservations struct {
	Host          reliabilityHostSampler
	WorktreeCount func(context.Context, *sqlite.Store) (int, error)
}

func startReliability(ctx context.Context, cfg config.Config, store *sqlite.Store, lifecycle sessionLifecycle, accounts ports.CodexAccountClientFactory, accountHome string, log *slog.Logger) (*httpd.ReliabilityDeps, error) {
	return startReliabilityWithObservations(ctx, cfg, store, lifecycle, accounts, accountHome, log, reliabilityObservations{
		Host: fleetpressure.NewHostSampler(), WorktreeCount: registeredWorktreeCount,
	})
}

// startReliabilityWithObservations retains quota, receipt and admission
// observation while requiring explicit native host and Git observation effects.
func startReliabilityWithObservations(ctx context.Context, cfg config.Config, store *sqlite.Store, lifecycle sessionLifecycle, accounts ports.CodexAccountClientFactory, accountHome string, log *slog.Logger, observations reliabilityObservations) (*httpd.ReliabilityDeps, error) {
	if observations.Host == nil || observations.WorktreeCount == nil {
		return nil, errors.New("reliability requires explicit host and worktree observers")
	}
	if !inferenceguard.Enabled() {
		return nil, nil
	}
	gate, err := inferenceguard.ForDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	admission, ok := lifecycle.(reliabilityAdmission)
	if !ok {
		return nil, errors.New("strict build requires worker admission controls")
	}
	if !filepath.IsAbs(accountHome) {
		return nil, errors.New("absolute native receipt root required")
	}
	o := &reliabilityObserver{store: store, admission: admission, gate: gate, host: observations.Host, worktreeCount: observations.WorktreeCount,
		receipts:  nativereceipts.NewObserver([]string{filepath.Join(accountHome, "sessions"), filepath.Join(accountHome, "archived_sessions")}, gate),
		autoscale: os.Getenv("AO_FLEET_AUTOSCALE") == "1"}
	admission.SetAdmissionDispatchCheck(func(ctx context.Context) error {
		err := gate.Check(ctx)
		// Provisioned non-inference workers have a separate occupancy control.
		// A full turn semaphore does not make the worker semaphore full.
		if errors.Is(err, inferenceguard.ErrCapacity) {
			return nil
		}
		return err
	})
	if o.autoscale {
		admission.SetWorkerResourceCeiling(0)
	}
	go func() {
		defer o.receipts.Close()
		o.observe(ctx)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.observe(ctx)
			}
		}
	}()
	log.Info("local reliability observer enabled", "autoscale", o.autoscale, "inference", "none")
	return &httpd.ReliabilityDeps{Snapshot: o.snapshot, VerifyAllowance: func(ctx context.Context) error { return verifyPostRefreshAllowance(ctx, gate, accounts, accountHome) }, UpdateWorkers: admission.UpdateAdmissionLimits,
		UpdateInference: func(ctx context.Context, c inferenceguard.Controls) error {
			return updateReliabilityInferenceControls(ctx, gate, c)
		}, Cancel: admission.CancelAdmissionRequest}, nil
}

// updateReliabilityInferenceControls persists the stop before attempting native
// interruption. Failure to acknowledge interruption never rolls back the stop.
func updateReliabilityInferenceControls(ctx context.Context, gate *inferenceguard.Gate, c inferenceguard.Controls) error {
	if err := gate.UpdateControls(ctx, c); err != nil {
		return err
	}
	// A policy change never clears EXHAUSTED or an uncertain dispatch.
	if c.EmergencyStop != nil && *c.EmergencyStop {
		if err := gate.InterruptActive(ctx); err != nil {
			return fmt.Errorf("emergency stop persisted; native interruption was not fully acknowledged; active work may remain: %w", err)
		}
	}
	return nil
}
func (o *reliabilityObserver) observe(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	now := time.Now().UTC()
	a, aerr := o.admission.AdmissionSnapshot(ctx)
	q, qerr := o.gate.Snapshot(ctx)
	m, herr := o.host.Sample(ctx, now)
	m.ActiveWorkers = a.ActiveWorkerSessions
	m.QueuedTasks = a.QueuedRequests
	m.QuotaState = strings.ToLower(q.Quota)
	if q.EmergencyStop || q.LatchedStop {
		m.QuotaState = "hard_stop"
	} else if q.Paused {
		m.QuotaState = "hard_stop"
	}
	var codes []string
	if aerr != nil {
		codes = append(codes, "admission_observation_failed")
	}
	if qerr != nil {
		codes = append(codes, "quota_observation_failed")
		m.QuotaState = "hard_stop"
	}
	if herr != nil {
		codes = append(codes, "host_observation_failed")
	}
	handles := o.host.HandleObservation()
	if !handles.FleetComplete {
		codes = append(codes, "fleet_handle_coverage_unknown")
	}
	// Detached/recovered hosts are absent from the own-tree handle sample.
	// Never use a partial low count as healthy expansion evidence. Its fresh
	// lower bound may still prove excessive pressure and block admission.
	m.ProcessHandles = handles.PressureLowerBound(now, fleetpressure.DefaultLimits(a.Limits.MaxActiveWorkers).MaxProcessHandles)
	sessions, err := o.store.ListAllSessions(ctx)
	if err != nil {
		codes = append(codes, "session_observation_failed")
	} else {
		var ids []domain.SessionID
		var bindings []nativereceipts.Binding
		for _, s := range sessions {
			ids = append(ids, s.ID)
			if s.Harness == domain.HarnessCodex {
				thread := s.Metadata.ProviderConversationID
				if thread == "" {
					thread = s.Metadata.AgentSessionID
				}
				bindings = append(bindings, nativereceipts.Binding{Session: string(s.ID), Thread: thread})
				bs, be := o.store.ListUsageBindingsForSession(ctx, s.ID)
				if be == nil {
					for _, b := range bs {
						if b.Harness == domain.HarnessCodex {
							bindings = append(bindings, nativereceipts.Binding{Session: string(s.ID), Thread: b.NativeRootID})
						}
					}
				}
			}
			reviews, re := o.store.ListReviewsBySession(ctx, s.ID)
			if re == nil {
				for _, r := range reviews {
					if r.Harness == domain.ReviewerCodex {
						thread := r.ProviderConversationID
						if thread == "" {
							thread = r.AgentSessionID
						}
						bindings = append(bindings, nativereceipts.Binding{Session: "review-" + r.ID, Thread: thread})
					}
				}
			}
		}
		facts, fe := o.store.ListPRFactsForSessions(ctx, ids)
		if fe == nil {
			unique := map[string]bool{}
			for _, rows := range facts {
				for _, pr := range rows {
					if pr.Merged {
						unique[pr.URL] = true
					}
				}
			}
			n := int64(len(unique))
			m.AcceptedResults = &n
		} else {
			codes = append(codes, "merge_outcomes_unknown")
		}
		registered, registryErr := o.gate.NativeThreads(ctx)
		if registryErr != nil {
			codes = append(codes, "native_thread_registry_unavailable")
		} else {
			// Ownership is resolved per admitted root turn inside the receipt authority.
			// One physical transcript can retain both a temporary fork host and its successor.
			bound := map[string]bool{}
			var authoritative []nativereceipts.Binding
			for _, n := range registered {
				if !bound[n.Thread] {
					bound[n.Thread] = true
					authoritative = append(authoritative, nativereceipts.Binding{Session: "native:" + n.Thread, Thread: n.Thread})
				}
			}
			for _, b := range bindings {
				if !bound[b.Thread] {
					authoritative = append(authoritative, b)
				}
			}
			bindings = authoritative
		}
		o.receipts.Sweep(ctx, bindings)
	}
	if o.worktreeCount == nil {
		codes = append(codes, "worktree_pressure_unknown")
	} else if n, err := o.worktreeCount(ctx, o.store); err == nil {
		m.Worktrees = &n
	} else {
		codes = append(codes, "worktree_pressure_unknown")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pressure == nil || o.ownerCeiling != a.Limits.MaxActiveWorkers {
		o.ownerCeiling = a.Limits.MaxActiveWorkers
		o.pressure = fleetpressure.New(fleetpressure.DefaultLimits(o.ownerCeiling))
	}
	d := o.pressure.Evaluate(time.Now().UTC(), m)
	if o.autoscale {
		o.admission.SetWorkerResourceCeiling(d.WorkerCeiling)
	}
	o.latest = d
	o.errors = codes
	o.merged = m.AcceptedResults
	o.at = now
}
func (o *reliabilityObserver) snapshot(ctx context.Context) (any, error) {
	a, err := o.admission.AdmissionSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	q, err := o.gate.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	queue, err := o.admission.ListAdmissionQueue(ctx)
	if err != nil {
		return nil, err
	}
	dispatches, err := o.gate.RecentDispatches(ctx)
	if err != nil {
		return nil, err
	}
	usage, err := o.gate.SessionUsage(ctx)
	if err != nil {
		return nil, err
	}
	outcomes, outcomeErr := reliabilityUsageOutcomes(ctx, o.store, usage)
	if outcomeErr != nil {
		outcomes = nil
	}
	health, healthErr := o.admission.HealthSnapshot(ctx)
	if healthErr != nil {
		health.Status = "UNKNOWN"
	}
	projects, projectErr := o.store.ListProjects(ctx)
	var projectIDs []string
	if projectErr == nil {
		for _, project := range projects {
			projectIDs = append(projectIDs, project.ID)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return map[string]any{"observedAt": o.at, "admission": a, "inference": q, "queue": queue, "dispatches": dispatches,
		"sessionUsage": usage, "sessionOutcomes": outcomes, "sessionOutcomesAvailable": outcomeErr == nil,
		"pressure": o.latest, "handleObservation": o.host.HandleObservation(), "autoscaleEnabled": o.autoscale, "mergedPRs": o.merged, "acceptedRequirements": nil, "idleWaste": nil,
		"retryAttribution": "failed dispatches measured; retry causality unavailable",
		"workspaceScripts": o.admission.WorkspaceScriptSnapshot(), "controllerHealth": health, "projects": projectIDs, "allowanceCheckEnabled": os.Getenv("AO_ENABLE_ALLOWANCE_CHECK") == "1",
		"receipts": o.receipts.Snapshot(), "errors": append([]string(nil), o.errors...)}, nil
}

// Git is a local read-only observation, with bounded context/output and no shell.
type boundedGitOutput struct {
	data  []byte
	limit int
}

func (w *boundedGitOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > w.limit {
		return 0, errors.New("git inventory exceeds observation budget")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func gitObservation(ctx context.Context, path string, args ...string) (string, error) {
	child, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	argv := append([]string{"-C", path}, args...)
	cmd := exec.CommandContext(child, "git", argv...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output := &boundedGitOutput{limit: 2 << 20}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return string(output.data), nil
}
func registeredWorktreeCount(ctx context.Context, store *sqlite.Store) (int, error) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	if len(projects) > 16 {
		return 0, errors.New("project inventory exceeds per-sweep budget")
	}
	repositories := map[string]bool{}
	trees := map[string]bool{}
	for _, p := range projects {
		if p.Kind == domain.ProjectKindScratch {
			continue
		}
		common, err := gitObservation(ctx, p.Path, "rev-parse", "--git-common-dir")
		if err != nil {
			return 0, err
		}
		common = strings.TrimSpace(common)
		if !filepath.IsAbs(common) {
			common = filepath.Join(p.Path, common)
		}
		if repositories[common] {
			continue
		}
		repositories[common] = true
		output, err := gitObservation(ctx, p.Path, "worktree", "list", "--porcelain")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(output, "\n") {
			if path, ok := strings.CutPrefix(line, "worktree "); ok {
				trees[strings.TrimSpace(path)] = true
			}
		}
	}
	return len(trees), nil
}
