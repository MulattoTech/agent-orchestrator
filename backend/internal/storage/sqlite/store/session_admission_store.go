package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const admissionColumns = "id, project_id, operation, session_id, payload, state, generation, owner, attempts, failure_code, created_at, updated_at, available_at"

// Admission commands use a transactional compare-and-swap claim. The SQL is
// kept beside the transaction so the claim's generation/owner predicates are
// reviewed together; no generated sqlc files are edited.
func (s *Store) EnqueueSessionAdmission(ctx context.Context, rec domain.SessionAdmissionRequest) error {
	if rec.ID == "" || rec.Payload == "" || (rec.Operation != "spawn" && rec.Operation != "restore") {
		return errors.New("invalid admission request")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	var existingProject, existingOperation, existingPayload string
	existingErr := s.writeDB.QueryRowContext(ctx, "SELECT project_id,operation,payload FROM session_admission_queue WHERE id=?", rec.ID).Scan(&existingProject, &existingOperation, &existingPayload)
	if existingErr == nil {
		if existingProject != string(rec.ProjectID) || existingOperation != rec.Operation || existingPayload != rec.Payload {
			return errors.New("admission request ID conflict")
		}
		return nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return existingErr
	}
	var count int
	if err := s.writeDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_admission_queue WHERE state IN ('queued','running')").Scan(&count); err != nil {
		return err
	}
	if count >= 1024 {
		return errors.New("session admission queue is full (1024 commands)")
	}
	_, err := s.writeDB.ExecContext(ctx, `INSERT INTO session_admission_queue
		(id,project_id,operation,session_id,payload,state,created_at,updated_at,available_at)
		VALUES (?,?,?,?,?,'queued',?,?,?) ON CONFLICT(id) DO NOTHING`,
		rec.ID, rec.ProjectID, rec.Operation, rec.SessionID, rec.Payload,
		admissionTime(rec.CreatedAt), admissionTime(rec.UpdatedAt), admissionTime(rec.AvailableAt))
	if err != nil {
		return err
	}
	var project, operation, payload string
	if err := s.writeDB.QueryRowContext(ctx, "SELECT project_id,operation,payload FROM session_admission_queue WHERE id=?", rec.ID).Scan(&project, &operation, &payload); err != nil {
		return err
	}
	if project != string(rec.ProjectID) || operation != rec.Operation || payload != rec.Payload {
		return errors.New("admission request ID conflict")
	}
	return nil
}

func admissionTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func scanAdmission(row interface{ Scan(...any) error }) (domain.SessionAdmissionRequest, error) {
	var rec domain.SessionAdmissionRequest
	var created, updated, available string
	err := row.Scan(&rec.ID, &rec.ProjectID, &rec.Operation, &rec.SessionID, &rec.Payload, &rec.State,
		&rec.Generation, &rec.Owner, &rec.Attempts, &rec.FailureCode, &created, &updated, &available)
	if err != nil {
		return rec, err
	}
	for _, value := range []struct {
		raw    string
		target *time.Time
	}{{created, &rec.CreatedAt}, {updated, &rec.UpdatedAt}, {available, &rec.AvailableAt}} {
		t, err := time.Parse(time.RFC3339Nano, value.raw)
		if err != nil {
			return rec, fmt.Errorf("admission timestamp: %w", err)
		}
		*value.target = t
	}
	return rec, nil
}

// GetSessionAdmission looks up the immutable command by its indexed identity.
func (s *Store) GetSessionAdmission(ctx context.Context, id string) (domain.SessionAdmissionRequest, bool, error) {
	rec, err := scanAdmission(s.readDB.QueryRowContext(ctx, "SELECT "+admissionColumns+" FROM session_admission_queue WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionAdmissionRequest{}, false, nil
	}
	return rec, err == nil, err
}

func (s *Store) ListSessionAdmission(ctx context.Context) ([]domain.SessionAdmissionRequest, error) {
	rows, err := s.readDB.QueryContext(ctx, "SELECT "+admissionColumns+" FROM session_admission_queue ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.SessionAdmissionRequest
	for rows.Next() {
		rec, err := scanAdmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ClaimSessionAdmission rotates projects using their last durable claim time,
// then preserves FIFO order within each project. Updating a claim and reading
// its payload share the single-writer transaction.
func (s *Store) ClaimSessionAdmission(ctx context.Context, owner string, now time.Time) (domain.SessionAdmissionRequest, bool, error) {
	if owner == "" {
		return domain.SessionAdmissionRequest{}, false, errors.New("admission claim requires owner")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.SessionAdmissionRequest{}, false, err
	}
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return domain.SessionAdmissionRequest{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `SELECT `+admissionColumns+` FROM session_admission_queue q
		WHERE state='queued' AND available_at<=?
		ORDER BY COALESCE((SELECT MAX(p.updated_at) FROM session_admission_queue p
		  WHERE p.project_id=q.project_id AND p.generation>0),''),
		q.created_at,q.id LIMIT 1`, admissionTime(now))
	rec, err := scanAdmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_admission_queue SET state='running',owner=?,generation=generation+1,updated_at=?
		WHERE id=? AND state='queued' AND generation=?`, owner, admissionTime(now), rec.ID, rec.Generation)
	if err != nil {
		return rec, false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return rec, false, err
	}
	if err := tx.Commit(); err != nil {
		return rec, false, err
	}
	rec.Owner = owner
	rec.State = "running"
	rec.Generation++
	rec.UpdatedAt = now
	return rec, true, nil
}

// CompleteSessionAdmission rejects late completions from an old daemon or
// cancelled generation. Uncertain outcomes never overwrite a newer owner.
func (s *Store) CompleteSessionAdmission(ctx context.Context, rec domain.SessionAdmissionRequest) (bool, error) {
	if rec.State != "queued" && rec.State != "completed" && rec.State != "blocked" {
		return false, errors.New("invalid admission completion")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	result, err := s.writeDB.ExecContext(ctx, `UPDATE session_admission_queue SET state=?,session_id=?,attempts=?,failure_code=?,updated_at=?,available_at=?
		WHERE id=? AND state='running' AND owner=? AND generation=?`,
		rec.State, rec.SessionID, rec.Attempts, rec.FailureCode, admissionTime(rec.UpdatedAt), admissionTime(rec.AvailableAt), rec.ID, rec.Owner, rec.Generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// RecoverSessionAdmission is called only by the exclusive daemon owner during
// pre-listener startup. A successful command whose ack was lost is replayed
// through its session client-request key; ambiguous partial starts are blocked.
func (s *Store) RecoverSessionAdmission(ctx context.Context, owner string, now time.Time) error {
	if owner == "" {
		return errors.New("admission recovery requires owner")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	_, err := s.writeDB.ExecContext(ctx, `UPDATE session_admission_queue SET state='queued',owner='',generation=generation+1,
		failure_code='DAEMON_RESTART',updated_at=?,available_at=? WHERE state='running' AND owner<>?`,
		admissionTime(now), admissionTime(now), owner)
	return err
}

func (s *Store) CancelSessionAdmission(ctx context.Context, id string, now time.Time) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	result, err := s.writeDB.ExecContext(ctx, `UPDATE session_admission_queue SET state='cancelled',generation=generation+1,updated_at=?
		WHERE id=? AND state IN ('queued','blocked')`, admissionTime(now), id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// SetSessionAdmissionLimits is the durable owner control. Existing workers are
// not mutated, archived or terminated when their configured ceiling decreases.
func (s *Store) SetSessionAdmissionLimits(ctx context.Context, limits domain.SessionAdmissionLimits, now time.Time) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(limits)
	if err != nil {
		return err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	_, err = s.writeDB.ExecContext(ctx, `INSERT INTO session_admission_settings (id,limits_json,updated_at) VALUES (1,?,?)
 ON CONFLICT(id) DO UPDATE SET limits_json=excluded.limits_json,updated_at=excluded.updated_at`, string(payload), admissionTime(now))
	return err
}

func (s *Store) GetSessionAdmissionLimits(ctx context.Context) (domain.SessionAdmissionLimits, bool, error) {
	var limits domain.SessionAdmissionLimits
	var payload string
	err := s.readDB.QueryRowContext(ctx, "SELECT limits_json FROM session_admission_settings WHERE id=1").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return limits, false, nil
	}
	if err != nil {
		return limits, false, err
	}
	if err := json.Unmarshal([]byte(payload), &limits); err != nil {
		return limits, false, fmt.Errorf("corrupt session admission settings: %w", err)
	}
	if err := limits.Validate(); err != nil {
		return limits, false, fmt.Errorf("invalid session admission settings: %w", err)
	}
	return limits, true, nil
}
