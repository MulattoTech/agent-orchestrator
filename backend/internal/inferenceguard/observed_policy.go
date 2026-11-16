package inferenceguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ObserveNativeSelection records a native model/effort drift after a trustworthy
// response receipt was persisted. It never relabels actual usage as requested
// usage and never calls a model. The returned policy error is post-dispatch,
// deliberately not a PreDispatchRefusal.
func (g *Gate) ObserveNativeSelection(ctx context.Context, session, turn, model, effort string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.current(); err != nil {
		return err
	}
	var expectedModel, expectedEffort string
	if err := g.db.QueryRowContext(ctx, "SELECT model,effort FROM attempts WHERE session_id=? AND provider_turn_id=? AND dispatched_at<>'' LIMIT 1", session, turn).Scan(&expectedModel, &expectedEffort); err != nil {
		return ErrUnboundNativeTurn
	}
	if model == expectedModel && strings.EqualFold(strings.TrimSpace(effort), expectedEffort) {
		return nil
	}
	var priorReview string
	priorErr := g.db.QueryRowContext(ctx, "SELECT reviewed_at FROM policy_violations WHERE session_id=? AND provider_turn_id=? AND observed_model=? AND observed_effort=?", session, turn, model, effort).Scan(&priorReview)
	if priorErr == nil && priorReview != "" {
		return nil
	}
	first := !g.policyBlocked.Swap(true)
	g.blocked.Store(true)
	if first {
		defer func() { go func() { _ = g.InterruptActive(context.Background()) }() }()
	}
	if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
		return priorErr
	}
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := g.now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO policy_violations(session_id,provider_turn_id,expected_model,expected_effort,observed_model,observed_effort,detected_at) VALUES(?,?,?,?,?,?,?)", session, turn, expectedModel, expectedEffort, model, effort, now)
	if err != nil {
		return err
	}
	var reviewed string
	if err = tx.QueryRowContext(ctx, "SELECT reviewed_at FROM policy_violations WHERE session_id=? AND provider_turn_id=? AND observed_model=? AND observed_effort=?", session, turn, model, effort).Scan(&reviewed); err != nil {
		return err
	}
	if reviewed != "" {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE authority SET status='POLICY_VIOLATION',updated_at=? WHERE id=1", now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return fmt.Errorf("%w: native model/effort differed from the admitted wire selection", ErrPolicy)
}

// ReviewPolicyViolation requires an explicit owner review after all governed
// work settled, with emergency stop still on. It leaves quota EXHAUSTED: fresh
// allowance evidence and a separate owner restart are required afterwards.
func (g *Gate) ReviewPolicyViolation(ctx context.Context, owner, reason string) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("policy review owner and evidence reason are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	if !p.EmergencyStop {
		return deny(ErrPolicy, "policy review requires emergency stop")
	}
	var active int
	if err = g.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE state IN ('active','uncertain')").Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return deny(ErrPolicy, "unsettled native work must be reconciled before policy review")
	}
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM authority WHERE id=1").Scan(&status); err != nil {
		return err
	}
	if status != "POLICY_VIOLATION" {
		return deny(ErrPolicy, "no durable policy violation is awaiting review")
	}
	now := g.now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, "UPDATE policy_violations SET reviewed_at=?,review_owner=?,review_reason=? WHERE reviewed_at=''", now, owner, reason); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE authority SET status='EXHAUSTED',updated_at=?,reset_source='owner_policy_review_requires_fresh_allowance' WHERE id=1", now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	g.policyBlocked.Store(false)
	g.blocked.Store(true)
	return nil
}
