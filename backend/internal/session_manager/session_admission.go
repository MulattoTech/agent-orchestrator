package sessionmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/google/uuid"
)

var (
	ErrAdmissionDependencyPending = errors.New("ADMISSION_DEPENDENCY_PENDING")
	ErrWorkerCapacity             = errors.New("SESSION_CAPACITY_REACHED")
	ErrSessionQueued              = errors.New("SESSION_CAPACITY_QUEUED")
	ErrOrchestratorOccupied       = errors.New("PROJECT_COORDINATOR_OCCUPIED")
	ErrWorkspaceOwned             = errors.New("WORKSPACE_WRITE_OWNER_CONFLICT")
	ErrWorkspaceOwnershipUnknown  = errors.New("WORKSPACE_OWNERSHIP_UNKNOWN")
)

// SessionQueuedError identifies the durable command; it does not pretend that
// a worker was created. Retrying the command with the same client key is safe.
type SessionQueuedError struct{ RequestID string }

func (e *SessionQueuedError) Error() string {
	return "SESSION_CAPACITY_QUEUED: durable request " + e.RequestID
}
func (e *SessionQueuedError) Unwrap() error { return ErrSessionQueued }

type admissionReservation struct {
	project   domain.ProjectID
	kind      domain.SessionKind
	id        domain.SessionID
	workspace string
}
type admissionContextKey struct{}
type admissionContext struct {
	manager     *Manager
	reservation *admissionReservation
}
type admissionQueueContextKey struct{}
type sessionAdmission struct {
	mu              sync.Mutex
	limits          domain.SessionAdmissionLimits
	resourceCeiling int
	reservations    map[*admissionReservation]struct{}
	owner           string
	dispatchMu      sync.Mutex
	dispatchStarted bool
	dispatchCheck   func(context.Context) error
}

// SetWorkerResourceCeiling only constrains the owner's configured ceiling.
// Lowering it never kills existing workers or deletes their worktrees.
func (m *Manager) SetWorkerResourceCeiling(ceiling int) {
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	m.admission.resourceCeiling = max(0, ceiling)
}

// SetAdmissionDispatchCheck installs a non-inference observer. When it refuses,
// queued commands remain untouched and no spawn/provider preflight is attempted.
func (m *Manager) SetAdmissionDispatchCheck(check func(context.Context) error) {
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	m.admission.dispatchCheck = check
}

func (m *Manager) reserveSessionAdmission(ctx context.Context, project domain.ProjectID, kind domain.SessionKind, id domain.SessionID) (context.Context, func(domain.SessionID), func(), error) {
	noop := func() {}
	if existing, ok := ctx.Value(admissionContextKey{}).(admissionContext); ok && existing.manager == m {
		return ctx, func(domain.SessionID) {}, noop, nil
	}
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ctx, nil, nil, err
	}
	if m.admission.limits.MaxActiveWorkers <= 0 {
		return ctx, func(domain.SessionID) {}, noop, nil
	}
	if kind != domain.KindWorker && kind != domain.KindOrchestrator {
		return ctx, nil, nil, fmt.Errorf("session admission: unsupported session kind %q", kind)
	}
	rows, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return ctx, nil, nil, fmt.Errorf("session admission read: %w", err)
	}
	active := make(map[domain.SessionID]domain.SessionRecord)
	global, projectCount := 0, 0
	for _, row := range rows {
		if row.IsTerminated || row.IsTaskPreparation {
			continue
		}
		active[row.ID] = row
		if row.Kind == domain.KindWorker {
			global++
			if row.ProjectID == project {
				projectCount++
			}
		}
		if kind == domain.KindOrchestrator && row.Kind == kind && row.ProjectID == project && row.ID != id {
			return ctx, nil, nil, ErrOrchestratorOccupied
		}
	}
	for r := range m.admission.reservations {
		if id != "" && r.id == id {
			return ctx, nil, nil, ErrOperationInProgress
		}
		if kind == domain.KindOrchestrator && r.kind == kind && r.project == project {
			return ctx, nil, nil, ErrOrchestratorOccupied
		}
		if r.kind != domain.KindWorker {
			continue
		}
		if _, counted := active[r.id]; r.id != "" && counted {
			continue
		}
		global++
		if r.project == project {
			projectCount++
		}
	}
	current, alreadyCounted := active[id]
	// Restarting a known stopped process expands actual running occupancy even
	// though its session row already exists. A reduced ceiling may defer it.
	if kind == domain.KindWorker && alreadyCounted && current.RuntimeReleased() {
		limit := m.admission.limits.MaxActiveWorkers
		if m.admission.resourceCeiling >= 0 {
			limit = min(limit, m.admission.resourceCeiling)
		}
		projectLimit := m.admission.limits.DefaultProjectWorkers
		if override, ok := m.admission.limits.ProjectWorkers[project]; ok {
			projectLimit = override
		}
		if limit == 0 || global > limit || (projectLimit > 0 && projectCount > projectLimit) {
			return ctx, nil, nil, fmt.Errorf("%w: stopped worker restart exceeds current ceiling", ErrWorkerCapacity)
		}
	}
	if kind == domain.KindWorker && !alreadyCounted {
		limit := m.admission.limits.MaxActiveWorkers
		if m.admission.resourceCeiling >= 0 {
			limit = min(limit, m.admission.resourceCeiling)
		}
		perProject := m.admission.limits.DefaultProjectWorkers
		if override, ok := m.admission.limits.ProjectWorkers[project]; ok {
			perProject = override
		}
		if global >= limit || (perProject > 0 && projectCount >= perProject) {
			return ctx, nil, nil, fmt.Errorf("%w: workers %d/%d; project %d/%d", ErrWorkerCapacity, global, limit, projectCount, perProject)
		}
	}
	r := &admissionReservation{project: project, kind: kind, id: id}
	m.admission.reservations[r] = struct{}{}
	bind := func(id domain.SessionID) { m.admission.mu.Lock(); r.id = id; m.admission.mu.Unlock() }
	var once sync.Once
	release := func() {
		once.Do(func() { m.admission.mu.Lock(); delete(m.admission.reservations, r); m.admission.mu.Unlock() })
	}
	return context.WithValue(ctx, admissionContextKey{}, admissionContext{m, r}), bind, release, nil
}

// reserveWorkspaceOwner fences aliases across projects as well as the ordinary
// per-project workspace lock. It never removes or rewrites the conflicting tree.
func (m *Manager) reserveWorkspaceOwner(ctx context.Context, id domain.SessionID, path string) error {
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	if m.admission.limits.MaxActiveWorkers <= 0 || strings.TrimSpace(path) == "" {
		return nil
	}
	requested, err := identifyAdmissionWorkspace(path)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve requested physical workspace: %v", ErrWorkspaceOwnershipUnknown, err)
	}
	rows, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID == id || row.IsTerminated || row.IsTaskPreparation || row.Metadata.WorkspacePath == "" {
			continue
		}
		owner, err := identifyAdmissionWorkspace(row.Metadata.WorkspacePath)
		if err != nil {
			return fmt.Errorf("%w: cannot resolve session %s workspace: %v", ErrWorkspaceOwnershipUnknown, row.ID, err)
		}
		if sameAdmissionWorkspace(requested, owner) {
			return fmt.Errorf("%w: %s", ErrWorkspaceOwned, row.ID)
		}
	}
	var own *admissionReservation
	if value, ok := ctx.Value(admissionContextKey{}).(admissionContext); ok && value.manager == m {
		own = value.reservation
	}
	for r := range m.admission.reservations {
		if r == own || r.workspace == "" {
			continue
		}
		owner, err := identifyAdmissionWorkspace(r.workspace)
		if err != nil {
			return fmt.Errorf("%w: cannot resolve reserved workspace: %v", ErrWorkspaceOwnershipUnknown, err)
		}
		if sameAdmissionWorkspace(requested, owner) {
			return ErrWorkspaceOwned
		}
	}
	if own != nil {
		own.workspace = path
	}
	return nil
}

type sessionAdmissionStore interface {
	EnqueueSessionAdmission(context.Context, domain.SessionAdmissionRequest) error
	ListSessionAdmission(context.Context) ([]domain.SessionAdmissionRequest, error)
	ClaimSessionAdmission(context.Context, string, time.Time) (domain.SessionAdmissionRequest, bool, error)
	CompleteSessionAdmission(context.Context, domain.SessionAdmissionRequest) (bool, error)
	RecoverSessionAdmission(context.Context, string, time.Time) error
	CancelSessionAdmission(context.Context, string, time.Time) (bool, error)
}

func (m *Manager) queueSpawnAdmission(ctx context.Context, cfg ports.SpawnConfig, cause error) error {
	if (!errors.Is(cause, ErrWorkerCapacity) && !errors.Is(cause, ErrAdmissionDependencyPending) && !errors.Is(cause, ErrPRCompletionPending)) || ctx.Value(admissionQueueContextKey{}) != nil {
		return cause
	}
	m.admission.mu.Lock()
	enabled := m.admission.limits.QueueEnabled
	m.admission.mu.Unlock()
	store, ok := m.store.(sessionAdmissionStore)
	if !ok || !enabled {
		return cause
	}
	if len(cfg.AdmissionDependencies) > 0 {
		existing, err := store.ListSessionAdmission(ctx)
		if err != nil {
			return err
		}
		known := make(map[string]bool)
		for _, request := range existing {
			known[request.ID] = true
		}
		for _, id := range cfg.AdmissionDependencies {
			if !known[id] {
				return fmt.Errorf("ADMISSION_DEPENDENCY_NOT_FOUND: %s", id)
			}
		}
	}
	id := cfg.ClientRequestID
	if id == "" {
		id = uuid.NewString()
	}
	sum := sha256.Sum256([]byte("spawn:" + string(cfg.ProjectID) + ":" + id))
	id = "admit-" + hex.EncodeToString(sum[:16])
	for _, dependency := range cfg.AdmissionDependencies {
		if dependency == id {
			return errors.New("ADMISSION_DEPENDENCY_CYCLE")
		}
	}
	// Speculative worktrees expire. A deferred command provisions its own tree.
	cfg.TaskPreparation = ""
	cfg.Async = false
	if cfg.ClientRequestID == "" {
		cfg.ClientRequestID = id
		bytes, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(bytes)
		cfg.ClientRequestHash = hex.EncodeToString(hash[:])
	}
	bytes, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	now := m.clock()
	rec := domain.SessionAdmissionRequest{ID: id, ProjectID: cfg.ProjectID, Operation: "spawn", Payload: string(bytes), CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	if err := store.EnqueueSessionAdmission(ctx, rec); err != nil {
		return fmt.Errorf("capacity request was NOT queued: %w", err)
	}
	return &SessionQueuedError{RequestID: id}
}

func (m *Manager) queueRestoreAdmission(ctx context.Context, rec domain.SessionRecord, cause error) error {
	if !errors.Is(cause, ErrWorkerCapacity) || ctx.Value(admissionQueueContextKey{}) != nil {
		return cause
	}
	m.admission.mu.Lock()
	enabled := m.admission.limits.QueueEnabled
	m.admission.mu.Unlock()
	store, ok := m.store.(sessionAdmissionStore)
	if !ok || !enabled {
		return cause
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("restore:%s:%d", rec.ID, rec.CleanupGeneration)))
	id := "admit-" + hex.EncodeToString(sum[:16])
	// Metadata timestamps can change during harmless rename/reconciliation.
	// The monotonic terminal episode is the restore intent's ownership fence.
	body, err := json.Marshal(struct {
		ID                domain.SessionID
		CleanupGeneration int64
	}{rec.ID, rec.CleanupGeneration})
	if err != nil {
		return err
	}
	now := m.clock()
	request := domain.SessionAdmissionRequest{ID: id, ProjectID: rec.ProjectID, SessionID: rec.ID, Operation: "restore", Payload: string(body), CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	if err := store.EnqueueSessionAdmission(ctx, request); err != nil {
		return fmt.Errorf("capacity request was NOT queued: %w", err)
	}
	return &SessionQueuedError{RequestID: id}
}

func (m *Manager) ListAdmissionQueue(ctx context.Context) ([]domain.SessionAdmissionRequest, error) {
	store, ok := m.store.(sessionAdmissionStore)
	if !ok {
		return nil, nil
	}
	requests, err := store.ListSessionAdmission(ctx)
	if err != nil {
		return nil, err
	}
	for i := range requests {
		if requests[i].Operation == "spawn" {
			var cfg ports.SpawnConfig
			if json.Unmarshal([]byte(requests[i].Payload), &cfg) == nil {
				requests[i].PRCompletionDependencies = cfg.PRCompletionDependencies
			}
		}
	}
	return requests, nil
}

func (m *Manager) CancelAdmissionRequest(ctx context.Context, id string) (bool, error) {
	store, ok := m.store.(sessionAdmissionStore)
	if !ok {
		return false, errors.New("admission queue unavailable")
	}
	return store.CancelSessionAdmission(ctx, id, m.clock())
}

func (m *Manager) AdmissionSnapshot(ctx context.Context) (domain.SessionAdmissionSnapshot, error) {
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	out := domain.SessionAdmissionSnapshot{Limits: cloneAdmissionLimits(m.admission.limits), ResourceWorkerCeiling: m.admission.resourceCeiling, ProjectWorkers: make(map[domain.ProjectID]int)}
	rows, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return out, err
	}
	active := make(map[domain.SessionID]bool)
	for _, row := range rows {
		if row.IsTerminated || row.IsTaskPreparation {
			continue
		}
		active[row.ID] = true
		switch row.Kind {
		case domain.KindWorker:
			out.ActiveWorkerSessions++
			out.ProjectWorkers[row.ProjectID]++
			if row.ProvisionState.IsProvisioning() {
				out.ProvisioningWorkers++
			}
		case domain.KindOrchestrator:
			out.LiveOrchestrators++
		}
	}
	for r := range m.admission.reservations {
		if r.kind == domain.KindWorker && !active[r.id] {
			out.ReservedWorkers++
		}
	}
	if store, ok := m.store.(sessionAdmissionStore); ok {
		requests, err := store.ListSessionAdmission(ctx)
		if err != nil {
			return out, err
		}
		for _, request := range requests {
			if request.State == "queued" || request.State == "running" {
				out.QueuedRequests++
			}
			if request.State == "blocked" {
				out.BlockedRequests++
			}
		}
	}
	return out, nil
}

// StartAdmissionDispatcher runs already-authorized deferred commands outside
// any controller lifetime. Startup reconciliation has to finish first. Waiting
// is a timer/SQLite read, never a model turn.
func (m *Manager) StartAdmissionDispatcher(ctx context.Context) {
	m.admission.mu.Lock()
	if m.admission.dispatchStarted {
		m.admission.mu.Unlock()
		return
	}
	m.admission.dispatchStarted = true
	m.admission.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-m.startupBackgroundReconcileDone:
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		lastPause := ""
		for {
			err := m.DispatchAdmissionQueue(ctx, 64)
			if err != nil && ctx.Err() == nil {
				if err.Error() != lastPause {
					m.logger.Warn("session capacity queue paused", "error", err)
				}
				lastPause = err.Error()
			} else if err == nil {
				lastPause = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// DispatchAdmissionQueue is also the deterministic offline test seam. It
// executes a bounded number of claims; model quota is checked by a non-model
// observer before claiming, and again at the final provider boundary.
func (m *Manager) DispatchAdmissionQueue(ctx context.Context, maxClaims int) error {
	store, ok := m.store.(sessionAdmissionStore)
	if !ok {
		return nil
	}
	m.admission.dispatchMu.Lock()
	defer m.admission.dispatchMu.Unlock()
	for range max(0, min(maxClaims, 64)) {
		m.admission.mu.Lock()
		check := m.admission.dispatchCheck
		owner := m.admission.owner
		enabled := m.admission.limits.QueueEnabled
		m.admission.mu.Unlock()
		if !enabled {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if check != nil {
			if err := check(ctx); err != nil {
				return err
			}
		}
		request, found, err := store.ClaimSessionAdmission(ctx, owner, m.clock())
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		commandCtx := context.WithValue(ctx, admissionQueueContextKey{}, request.ID)
		var session domain.SessionRecord
		switch request.Operation {
		case "spawn":
			var cfg ports.SpawnConfig
			if err = json.Unmarshal([]byte(request.Payload), &cfg); err == nil {
				err = m.admissionDependenciesReady(ctx, cfg.AdmissionDependencies)
				if err == nil {
					session, _, _, err = m.Spawn(commandCtx, cfg)
				}
			}
		case "restore":
			var wanted struct {
				ID                domain.SessionID
				CleanupGeneration int64
			}
			if err = json.Unmarshal([]byte(request.Payload), &wanted); err == nil {
				var found bool
				session, found, err = m.store.GetSession(ctx, wanted.ID)
				if err == nil && !found {
					err = ErrNotFound
				}
				if err == nil && session.IsTerminated && session.CleanupGeneration != wanted.CleanupGeneration {
					err = errors.New("STALE_RESTORE_REQUEST")
				}
				if err == nil && session.IsTerminated {
					var result RestoreResult
					result, err = m.RestoreWithMode(commandCtx, wanted.ID)
					session = result.Session
				}
			}
		default:
			err = errors.New("INVALID_ADMISSION_OPERATION")
		}
		now := m.clock()
		request.UpdatedAt = now
		request.AvailableAt = now
		switch {
		case err == nil:
			request.State = "completed"
			request.SessionID = session.ID
			request.FailureCode = ""
		case errors.Is(err, ErrPRCompletionPending):
			request.State = "queued"
			request.FailureCode = "PR_COMPLETION_PENDING"
			request.AvailableAt = now.Add(10 * time.Second)
		case errors.Is(err, ErrAdmissionDependencyPending):
			request.State = "queued"
			request.FailureCode = "ADMISSION_DEPENDENCY_PENDING"
			request.AvailableAt = now.Add(2 * time.Second)
		case errors.Is(err, ErrWorkerCapacity):
			request.State = "queued"
			request.FailureCode = "SESSION_CAPACITY_REACHED"
			request.AvailableAt = now.Add(2 * time.Second)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClientRequestIncomplete):
			// A process might have started before cancellation. Never retry an
			// ambiguous command with a fresh identity.
			request.State = "blocked"
			request.FailureCode = "OUTCOME_REQUIRES_RECONCILIATION"
		default:
			request.Attempts++
			request.State = "blocked"
			request.FailureCode = "ADMISSION_COMMAND_FAILED"
			// Only an explicit operation-contention refusal is proven pre-launch.
			// One retry, bounded jitter, then a durable blocked command.
			if errors.Is(err, ErrOperationInProgress) && request.Attempts <= 1 {
				request.State = "queued"
				request.FailureCode = "SESSION_OPERATION_IN_PROGRESS"
				request.AvailableAt = now.Add(3*time.Second + time.Duration(request.Generation%7)*100*time.Millisecond)
			}
		}
		// Completion bookkeeping must survive request cancellation; it cannot
		// submit inference or modify session/worktree content.
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		applied, ackErr := store.CompleteSessionAdmission(ackCtx, request)
		cancel()
		if ackErr != nil {
			return ackErr
		}
		if !applied {
			return errors.New("admission generation changed before completion")
		}
	}
	return nil
}

// Existing request IDs only: because commands are immutable, a newly queued
// command cannot create a cycle through an older command.
func (m *Manager) admissionDependenciesReady(ctx context.Context, dependencies []string) error {
	if len(dependencies) == 0 {
		return nil
	}
	requests, err := m.ListAdmissionQueue(ctx)
	if err != nil {
		return err
	}
	states := make(map[string]string)
	for _, request := range requests {
		states[request.ID] = request.State
	}
	for _, id := range dependencies {
		switch states[id] {
		case "completed":
		case "queued", "running":
			return ErrAdmissionDependencyPending
		case "blocked", "cancelled":
			return errors.New("ADMISSION_DEPENDENCY_FAILED")
		default:
			return errors.New("ADMISSION_DEPENDENCY_NOT_FOUND")
		}
	}
	return nil
}
