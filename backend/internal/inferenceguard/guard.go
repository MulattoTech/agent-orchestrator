package inferenceguard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
	_ "modernc.org/sqlite"
)

type Request struct {
	ID        string
	Session   string
	Project   string
	Operation string
	Selection Selection
}
type Gate struct {
	path      string
	owner     *datadirlock.Lock
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	blocked   atomic.Bool
	stopMu    sync.Mutex
	stopNext  uint64
	stoppers  map[uint64]func(context.Context) error
	stopping  atomic.Bool
	db        *sql.DB
	policy    func() (Policy, error)
	now       func() time.Time
}

var gates struct {
	sync.Mutex
	byPath map[string]*Gate
}

// ForDataDir returns the daemon-wide authority; nil means a non-strict upstream build.
// All governed data roots use the same owner-selected shared account authority.
func ForDataDir(dataDir string) (*Gate, error) {
	if !Enabled() {
		return nil, nil
	}
	if !filepath.IsAbs(dataDir) {
		return nil, deny(ErrPolicy, "strict dispatch requires an absolute isolated AO data root")
	}
	p, err := ReadPolicy()
	if err != nil {
		return nil, err
	}
	if p.AuthorityPath == "" {
		return nil, deny(ErrPolicy, "a shared absolute authority_path is required")
	}
	path, err := canonicalAuthorityPath(p.AuthorityPath)
	if err != nil {
		return nil, err
	}
	gates.Lock()
	defer gates.Unlock()
	if g := gates.byPath[path]; g != nil {
		return g, nil
	}
	g, err := Open(path, ReadPolicy, time.Now)
	if err != nil {
		return nil, err
	}
	if gates.byPath == nil {
		gates.byPath = map[string]*Gate{}
	}
	gates.byPath[path] = g
	return g, nil
}

// Open has no provider side effects. Tests pass a fake policy and clock.
func Open(path string, policy func() (Policy, error), now func() time.Time) (*Gate, error) {
	if policy == nil || now == nil {
		return nil, errors.New("policy and clock are required")
	}
	p, err := policy()
	if err != nil {
		return nil, err
	}
	if err = p.Validate(); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	uri := url.URL{Path: path}
	db, err := sql.Open("sqlite", "file:"+uri.EscapedPath()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	owner, lockErr := datadirlock.Acquire(context.Background(), path+".owner", 0)
	if lockErr != nil {
		_ = db.Close()
		return nil, deny(ErrPolicy, "shared provider/account authority is already owned or cannot be locked")
	}
	g := &Gate{db: db, policy: policy, now: now, path: path, owner: owner}
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		_ = owner.Release()
		return nil, err
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO authority(id,account_key,status,updated_at) VALUES(1,?,'UNKNOWN',?)", p.AccountKey, now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = db.Close()
		_ = owner.Release()
		return nil, err
	}
	var account string
	if err = db.QueryRow("SELECT account_key FROM authority WHERE id=1").Scan(&account); err != nil || account != p.AccountKey {
		_ = db.Close()
		_ = owner.Release()
		return nil, deny(ErrPolicy, "configured account does not match the durable authority; account switching is not an allowance workaround")
	}
	// A process restart cannot prove whether a committed admission reached the
	// provider. Keep its capacity reserved until exact native recovery evidence.
	if _, err = db.Exec("UPDATE attempts SET state='uncertain',error_class='process_restart' WHERE state='active'"); err != nil {
		_ = db.Close()
		_ = owner.Release()
		return nil, err
	}
	return g, nil
}
func (g *Gate) Close() error {
	g.closeOnce.Do(func() {
		gates.Lock()
		for key, value := range gates.byPath {
			if value == g {
				delete(gates.byPath, key)
			}
		}
		gates.Unlock()
		g.closeErr = errors.Join(g.db.Close(), g.owner.Release())
	})
	return g.closeErr
}

func canonicalAuthorityPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", deny(ErrPolicy, "authority path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS authority(
 id INTEGER PRIMARY KEY CHECK(id=1), account_key TEXT NOT NULL, status TEXT NOT NULL,
 reset_at TEXT NOT NULL DEFAULT '', reset_source TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS account_binding(id INTEGER PRIMARY KEY CHECK(id=1),native_sha256 TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS attempts(
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, project_id TEXT NOT NULL,
 account_key TEXT NOT NULL, epoch TEXT NOT NULL, role TEXT NOT NULL,
 requested_model TEXT NOT NULL, requested_effort TEXT NOT NULL,
 model TEXT NOT NULL, effort TEXT NOT NULL, exception_id TEXT NOT NULL,
 operation TEXT NOT NULL, state TEXT NOT NULL, created_at TEXT NOT NULL,
 dispatched_at TEXT NOT NULL DEFAULT '', provider_turn_id TEXT NOT NULL DEFAULT '',
 error_class TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS attempts_active ON attempts(state,project_id,role);
CREATE INDEX IF NOT EXISTS attempts_turn ON attempts(session_id,provider_turn_id);
CREATE TABLE IF NOT EXISTS dispatch_events(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, attempt_id TEXT NOT NULL,
 event TEXT NOT NULL, timestamp TEXT NOT NULL, detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_receipts(
 source_key TEXT PRIMARY KEY, provider_turn_id TEXT NOT NULL, session_id TEXT NOT NULL,
 account_key TEXT NOT NULL, model TEXT NOT NULL, effort TEXT NOT NULL,
 observed_at TEXT NOT NULL, input_tokens INTEGER NOT NULL, cached_input_tokens INTEGER NOT NULL,
 output_tokens INTEGER NOT NULL, reasoning_output_tokens INTEGER NOT NULL,
 confidence TEXT NOT NULL, payload TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS usage_receipts_no_update BEFORE UPDATE ON usage_receipts
 BEGIN SELECT RAISE(ABORT,'usage receipts are immutable'); END;
CREATE TRIGGER IF NOT EXISTS usage_receipts_no_delete BEFORE DELETE ON usage_receipts
 BEGIN SELECT RAISE(ABORT,'usage receipts are immutable'); END;
PRAGMA user_version=1;
`

func (g *Gate) current() (Policy, error) {
	p, err := g.policy()
	if err != nil {
		return p, err
	}
	if err = p.Validate(); err != nil {
		return p, err
	}
	if p.AuthorityPath != "" {
		path, pathErr := canonicalAuthorityPath(p.AuthorityPath)
		if pathErr != nil || path != g.path {
			return p, deny(ErrPolicy, "authority path changed while controller is open")
		}
	}
	var account string
	if err = g.db.QueryRow("SELECT account_key FROM authority WHERE id=1").Scan(&account); err != nil {
		return p, err
	}
	if account != p.AccountKey {
		return p, deny(ErrPolicy, "account authority binding changed")
	}
	return p, nil
}
func (g *Gate) Resolve(role, session, model, effort string) (Selection, error) {
	p, err := g.current()
	if err != nil {
		return Selection{}, err
	}
	return p.Resolve(role, session, model, effort, g.now())
}

// Dispatch records durable admission before writing, then serializes only the final
// write with quota notifications. It never holds the lock waiting for a model reply.
// On any ambiguous write error the lease remains occupied until verified recovery.
func (g *Gate) Dispatch(ctx context.Context, r Request, write func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	selected, err := p.Resolve(r.Selection.Role, r.Session, r.Selection.RequestedModel, r.Selection.RequestedEffort, g.now())
	if err != nil {
		return err
	}
	if selected.Model != r.Selection.Model || selected.Effort != r.Selection.Effort || selected.ExceptionID != r.Selection.ExceptionID {
		return deny(ErrPolicy, "policy changed during request preparation; prepare again")
	}
	if r.ID == "" || r.Session == "" || r.Operation == "" || write == nil {
		return deny(ErrPolicy, "dispatch identity is incomplete")
	}
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire SQLite's writer lock before reading caps. This is also cross-process.
	if _, err = tx.ExecContext(ctx, "UPDATE authority SET updated_at=updated_at WHERE id=1"); err != nil {
		return err
	}
	now := g.now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO attempts
 (id,session_id,project_id,account_key,epoch,role,requested_model,requested_effort,model,effort,exception_id,operation,state,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'queued',?)`,
		r.ID, r.Session, r.Project, p.AccountKey, p.AuthorizationEpoch, selected.Role, selected.RequestedModel, selected.RequestedEffort, selected.Model, selected.Effort, selected.ExceptionID, r.Operation, now)
	if err != nil {
		return err
	}
	var state, session, project, role, model, effort, operation, requestedModel, requestedEffort string
	if err = tx.QueryRowContext(ctx, "SELECT state,session_id,project_id,role,model,effort,operation,requested_model,requested_effort FROM attempts WHERE id=?", r.ID).Scan(&state, &session, &project, &role, &model, &effort, &operation, &requestedModel, &requestedEffort); err != nil {
		return err
	}
	if session != r.Session || project != r.Project || role != selected.Role || operation != r.Operation {
		return deny(ErrPolicy, "dispatch id was reused for different work")
	}
	if state != "queued" {
		return deny(ErrUncertain, "dispatch identity already admitted; reconcile the existing receipt instead of repeating it")
	}
	// Owner corrections may change an undispatched selection; durable request,
	// session, role and project identity stay fixed. No admitted receipt is rewritten.
	if _, err = tx.ExecContext(ctx, "UPDATE attempts SET requested_model=?,requested_effort=?,model=?,effort=? WHERE id=? AND state='queued'", selected.RequestedModel, selected.RequestedEffort, selected.Model, selected.Effort, r.ID); err != nil {
		return err
	}
	refusal := func(e error) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return e
	}
	if g.blocked.Load() {
		return refusal(deny(ErrQuota, "provider exhaustion observed; lockout is latched"))
	}
	if p.EmergencyStop {
		return refusal(deny(ErrQuota, "owner emergency stop; no provider dispatch"))
	}
	if p.Pause {
		return refusal(deny(ErrQuota, "owner soft pause; work remains queued"))
	}
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM authority WHERE id=1").Scan(&status); err != nil {
		return err
	}
	if status == "EXHAUSTED" {
		return refusal(deny(ErrQuota, "global provider/account lockout; reset time alone does not authorize retries"))
	}
	var spent, active, projectActive, reviews int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE epoch=? AND dispatched_at<>''", p.AuthorizationEpoch).Scan(&spent); err != nil {
		return err
	}
	if spent >= p.TurnBudget-p.ReserveTurns {
		return refusal(deny(ErrQuota, "conservative turn budget reached; real remaining quota is UNKNOWN"))
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE WHEN project_id=? THEN 1 ELSE 0 END),0),coalesce(sum(CASE WHEN role='reviewer' THEN 1 ELSE 0 END),0) FROM attempts WHERE state IN ('active','uncertain')`, r.Project).Scan(&active, &projectActive, &reviews); err != nil {
		return err
	}
	if active >= p.MaximumInference || projectActive >= p.MaximumPerProject || (selected.Role == "reviewer" && reviews >= p.MaximumReviewers) {
		return refusal(deny(ErrCapacity, "inference capacity occupied; session slots are independent"))
	}
	if _, err = tx.ExecContext(ctx, "UPDATE attempts SET state='active',dispatched_at=?,epoch=?,exception_id=? WHERE id=? AND state='queued'", now, p.AuthorizationEpoch, selected.ExceptionID, r.ID); err != nil {
		return err
	}
	resolution, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO dispatch_events(attempt_id,event,timestamp,detail) VALUES(?,'dispatch',?,?)", r.ID, now, string(resolution)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if g.blocked.Load() {
		_, persistErr := g.db.ExecContext(context.WithoutCancel(ctx), "UPDATE attempts SET state='queued',dispatched_at='' WHERE id=?", r.ID)
		if persistErr != nil {
			return persistErr
		}
		return deny(ErrQuota, "provider exhaustion observed before wire write")
	}
	if err = write(); err != nil {
		_, persistErr := g.db.ExecContext(context.WithoutCancel(ctx), "UPDATE attempts SET state='uncertain',error_class='transport' WHERE id=?", r.ID)
		return errors.Join(fmt.Errorf("%w: %v", ErrUncertain, err), persistErr)
	}
	return nil
}

func (g *Gate) Bind(ctx context.Context, id, turn string) error {
	if turn == "" {
		return errors.New("provider did not return a turn id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	result, err := g.db.ExecContext(ctx, "UPDATE attempts SET provider_turn_id=? WHERE id=? AND (provider_turn_id='' OR provider_turn_id=?)", turn, id, turn)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("dispatch receipt missing or already bound to a different native turn")
	}
	return nil
}

// Complete is idempotent; settlement of an exact bound turn returns capacity.
func (g *Gate) Complete(ctx context.Context, session, turn, errorClass string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.ExecContext(ctx, "UPDATE attempts SET state='completed',error_class=? WHERE session_id=? AND provider_turn_id=? AND state IN ('active','uncertain')", errorClass, session, turn)
	return err
}

// SettleRequest can be used only for a definitively rejected RPC or a complete
// native turn observed before its start acknowledgement has reached the caller.
func (g *Gate) SettleRequest(ctx context.Context, id, errorClass string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.ExecContext(ctx, "UPDATE attempts SET state='completed',error_class=? WHERE id=? AND state IN ('active','uncertain')", errorClass, id)
	return err
}

// Trip is durable and global. No timer or retry clears EXHAUSTED.
func (g *Gate) Trip(ctx context.Context, resetAt time.Time, source string) error {
	first := !g.blocked.Swap(true)
	g.mu.Lock()
	defer g.mu.Unlock()
	if first {
		defer func() { go func() { _ = g.InterruptActive(context.Background()) }() }()
	}
	reset := ""
	if !resetAt.IsZero() {
		reset = resetAt.UTC().Format(time.RFC3339Nano)
	}
	// Later unstructured errors must not erase an earlier authoritative reset.
	_, err := g.db.ExecContext(ctx, "UPDATE authority SET status='EXHAUSTED',reset_at=CASE WHEN ?<>'' AND (reset_at='' OR ?>reset_at) THEN ? ELSE reset_at END,reset_source=CASE WHEN ?<>'' AND (reset_at='' OR ?>reset_at) OR reset_at='' THEN ? ELSE reset_source END,updated_at=? WHERE id=1", reset, reset, reset, reset, reset, source, g.now().UTC().Format(time.RFC3339Nano))
	return err
}

// ConfirmAllowance requires an explicit non-inference observation after lockout.
// An ambiguous text reset is retained as a hint, never converted into permission.
func (g *Gate) ConfirmAllowance(ctx context.Context, account, source string, observedAt time.Time, available bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	if !available || account != p.AccountKey || source == "" || observedAt.IsZero() || observedAt.After(g.now()) || g.now().Sub(observedAt) > 5*time.Minute {
		return deny(ErrQuota, "fresh non-inference allowance evidence is required")
	}
	var updated, reset string
	if err = g.db.QueryRowContext(ctx, "SELECT updated_at,reset_at FROM authority WHERE id=1").Scan(&updated, &reset); err != nil {
		return err
	}
	blockedAt, _ := time.Parse(time.RFC3339Nano, updated)
	if !observedAt.After(blockedAt) {
		return deny(ErrQuota, "allowance evidence predates lockout")
	}
	if reset != "" {
		at, e := time.Parse(time.RFC3339Nano, reset)
		if e != nil || observedAt.Before(at) {
			return deny(ErrQuota, "authoritative reset is not yet reached")
		}
	}
	_, err = g.db.ExecContext(ctx, "UPDATE authority SET status='AVAILABLE',updated_at=?,reset_source=? WHERE id=1", observedAt.UTC().Format(time.RFC3339Nano), source)
	if err == nil {
		g.blocked.Store(false)
	}
	return err
}

// Check is a non-inference lockout read used by steering and observer admission.
func (g *Gate) Check(ctx context.Context) error {
	p, err := g.current()
	if err != nil {
		return err
	}
	if g.blocked.Load() || p.EmergencyStop || p.Pause {
		return deny(ErrQuota, "owner dispatch pause or latched provider lockout")
	}
	var status string
	if err = g.db.QueryRowContext(ctx, "SELECT status FROM authority WHERE id=1").Scan(&status); err != nil {
		return err
	}
	if status == "EXHAUSTED" {
		return deny(ErrQuota, "global provider/account lockout")
	}
	var spent, active int
	if err = g.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE epoch=? AND dispatched_at<>''", p.AuthorizationEpoch).Scan(&spent); err != nil {
		return err
	}
	if spent >= p.TurnBudget-p.ReserveTurns {
		return deny(ErrQuota, "conservative turn budget reached; remaining provider allowance is UNKNOWN")
	}
	if err = g.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE state IN ('active','uncertain')").Scan(&active); err != nil {
		return err
	}
	if active >= p.MaximumInference {
		return deny(ErrCapacity, "global inference capacity occupied")
	}
	return nil
}

type UsageReceipt struct {
	SourceKey             string    `json:"source_key"`
	ProviderTurnID        string    `json:"provider_turn_id"`
	Session               string    `json:"session"`
	Model                 string    `json:"model"`
	Effort                string    `json:"effort"`
	ObservedAt            time.Time `json:"observed_at"`
	InputTokens           int64     `json:"input_tokens"`
	CachedInputTokens     int64     `json:"cached_input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	ReasoningOutputTokens int64     `json:"reasoning_output_tokens"`
	Confidence            string    `json:"confidence"`
}

// IngestReceipt accepts only independent provider response receipts. Cumulative
// thread/turn context counters are intentionally rejected, never billed as deltas.
func (g *Gate) IngestReceipt(ctx context.Context, r UsageReceipt) error {
	if r.SourceKey == "" || r.ProviderTurnID == "" || r.Session == "" || r.ObservedAt.IsZero() || r.Model == "" || r.Effort == "" || r.Confidence != "provider_response" {
		return errors.New("independent per-response usage identity and provenance required")
	}
	if r.InputTokens < 0 || r.CachedInputTokens < 0 || r.OutputTokens < 0 || r.ReasoningOutputTokens < 0 || r.CachedInputTokens > r.InputTokens || r.ReasoningOutputTokens > r.OutputTokens {
		return errors.New("invalid per-response token counters")
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	_, err = g.db.ExecContext(ctx, `INSERT OR IGNORE INTO usage_receipts
 (source_key,provider_turn_id,session_id,account_key,model,effort,observed_at,input_tokens,cached_input_tokens,output_tokens,reasoning_output_tokens,confidence,payload)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.SourceKey, r.ProviderTurnID, r.Session, p.AccountKey, r.Model, r.Effort, r.ObservedAt.UTC().Format(time.RFC3339Nano), r.InputTokens, r.CachedInputTokens, r.OutputTokens, r.ReasoningOutputTokens, r.Confidence, string(payload))
	if err != nil {
		return err
	}
	var stored string
	if err = g.db.QueryRowContext(ctx, "SELECT payload FROM usage_receipts WHERE source_key=?", r.SourceKey).Scan(&stored); err != nil {
		return err
	}
	var prior UsageReceipt
	if err := json.Unmarshal([]byte(stored), &prior); err != nil {
		return err
	}
	// Re-observation time is not part of an immutable provider response identity.
	// Keep the first persisted timestamp while comparing all attribution/counters.
	prior.ObservedAt = r.ObservedAt
	semantic, err := json.Marshal(prior)
	if err != nil {
		return err
	}
	if string(semantic) != string(payload) {
		return errors.New("usage source key conflict; immutable receipt differs")
	}
	return nil
}

type ModelUsage struct {
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Turns           int    `json:"turns"`
	Failed          int    `json:"failed"`
	Receipts        int    `json:"receipts"`
	Input           int64  `json:"input_tokens"`
	CachedInput     int64  `json:"cached_input_tokens"`
	Output          int64  `json:"output_tokens"`
	ReasoningOutput int64  `json:"reasoning_output_tokens"`
}
type Snapshot struct {
	AccountKey                 string       `json:"account_key"`
	Quota                      string       `json:"quota"`
	ResetAt                    string       `json:"reset_at"`
	ResetSource                string       `json:"reset_source"`
	EmergencyStop              bool         `json:"emergency_stop"`
	Paused                     bool         `json:"paused"`
	InferenceCap               int          `json:"inference_cap"`
	ProjectCap                 int          `json:"project_cap"`
	ReviewerCap                int          `json:"reviewer_cap"`
	TurnBudget                 int          `json:"turn_budget"`
	ReserveTurns               int          `json:"reserve_turns"`
	BudgetUsed                 int          `json:"budget_used"`
	ConservativeTurnsRemaining int          `json:"conservative_turns_remaining"`
	LatchedStop                bool         `json:"latched_stop"`
	Active                     int          `json:"active"`
	Queued                     int          `json:"queued"`
	Uncertain                  int          `json:"uncertain"`
	Completed                  int          `json:"completed"`
	Models                     []ModelUsage `json:"models"`
	TelemetryConfidence        string       `json:"telemetry_confidence"`
}

func (g *Gate) Snapshot(ctx context.Context) (Snapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{AccountKey: p.AccountKey, EmergencyStop: p.EmergencyStop, Paused: p.Pause, InferenceCap: p.MaximumInference, ProjectCap: p.MaximumPerProject, ReviewerCap: p.MaximumReviewers, TurnBudget: p.TurnBudget, ReserveTurns: p.ReserveTurns, TelemetryConfidence: "provider receipts where present; missing telemetry is UNKNOWN; no dollar attribution"}
	if err = g.db.QueryRowContext(ctx, "SELECT status,reset_at,reset_source FROM authority WHERE id=1").Scan(&s.Quota, &s.ResetAt, &s.ResetSource); err != nil {
		return s, err
	}
	if err = g.db.QueryRowContext(ctx, `SELECT coalesce(sum(state='active'),0),coalesce(sum(state='queued'),0),coalesce(sum(state='uncertain'),0),coalesce(sum(state='completed'),0) FROM attempts`).Scan(&s.Active, &s.Queued, &s.Uncertain, &s.Completed); err != nil {
		return s, err
	}
	s.LatchedStop = g.blocked.Load() || s.Quota == "EXHAUSTED"
	if err = g.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE epoch=? AND dispatched_at<>''", p.AuthorizationEpoch).Scan(&s.BudgetUsed); err != nil {
		return s, err
	}
	s.ConservativeTurnsRemaining = max(0, p.TurnBudget-p.ReserveTurns-s.BudgetUsed)
	rows, err := g.db.QueryContext(ctx, `WITH classes AS (SELECT model,effort FROM attempts WHERE dispatched_at<>'' UNION SELECT model,effort FROM usage_receipts)
 SELECT classes.model,classes.effort,
 (SELECT count(*) FROM attempts a WHERE a.model=classes.model AND a.effort=classes.effort AND dispatched_at<>''),
 (SELECT count(*) FROM attempts a WHERE a.model=classes.model AND a.effort=classes.effort AND dispatched_at<>'' AND error_class<>'')
 FROM classes ORDER BY classes.model,classes.effort`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var m ModelUsage
		if err = rows.Scan(&m.Model, &m.Effort, &m.Turns, &m.Failed); err != nil {
			_ = rows.Close()
			return s, err
		}
		s.Models = append(s.Models, m)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return s, err
	}
	_ = rows.Close()
	for i := range s.Models {
		m := &s.Models[i]
		if err = g.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(cached_input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(reasoning_output_tokens),0) FROM usage_receipts WHERE model=? AND effort=?`, m.Model, m.Effort).Scan(&m.Receipts, &m.Input, &m.CachedInput, &m.Output, &m.ReasoningOutput); err != nil {
			return s, err
		}
	}
	return s, nil
}

// VerifyAccount compares a non-inference native identity observation against
// the owner's expected fingerprint and the immutable account binding.
func (g *Gate) VerifyAccount(ctx context.Context, fingerprint string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	if p.NativeAccountSHA256 == "" || fingerprint == "" || fingerprint != p.NativeAccountSHA256 {
		return deny(ErrPolicy, "native account identity is unknown or differs from owner policy")
	}
	if _, err = g.db.ExecContext(ctx, "INSERT OR IGNORE INTO account_binding(id,native_sha256) VALUES(1,?)", fingerprint); err != nil {
		return err
	}
	var expected string
	if err = g.db.QueryRowContext(ctx, "SELECT native_sha256 FROM account_binding WHERE id=1").Scan(&expected); err != nil {
		return err
	}
	if expected != fingerprint {
		return deny(ErrPolicy, "native identity differs from durable quota account")
	}
	return nil
}
