package inferenceguard

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrUnboundNativeTurn = errors.New("native turn has no unique verified account dispatch binding")

type NativeThreadBinding struct {
	Session    string `json:"session"`
	Thread     string `json:"thread"`
	AccountKey string `json:"account_key"`
}

// RegisterNativeThread records only a native thread returned after account verification.
// Historical host transfers are retained; each admitted request separately binds its
// exact native thread, so a temporary fork host cannot steal the final host's receipts.
func (g *Gate) RegisterNativeThread(ctx context.Context, session, thread string) error {
	if strings.TrimSpace(session) == "" || strings.TrimSpace(thread) == "" || len(session) > 512 || len(thread) > 512 {
		return deny(ErrPolicy, "native thread binding identity is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return err
	}
	if err := g.verifyAccountBinding(ctx, p); err != nil {
		return err
	}
	now := g.now().UTC().Format(time.RFC3339Nano)
	_, err = g.db.ExecContext(ctx, "INSERT INTO native_thread_bindings(session_id,thread_id,account_key,created_at,last_observed_at) VALUES(?,?,?,?,?) ON CONFLICT(session_id,thread_id) DO UPDATE SET last_observed_at=excluded.last_observed_at", session, thread, p.AccountKey, now, now)
	return err
}
func (g *Gate) verifyAccountBinding(ctx context.Context, p Policy) error {
	var fingerprint string
	if p.NativeAccountSHA256 == "" {
		return deny(ErrPolicy, "native account identity has not been configured")
	}
	if err := g.db.QueryRowContext(ctx, "SELECT native_sha256 FROM account_binding WHERE id=1").Scan(&fingerprint); err != nil || fingerprint != p.NativeAccountSHA256 {
		return deny(ErrPolicy, "native account has not been verified against durable authority")
	}
	return nil
}
func (g *Gate) verifyNativeThread(ctx context.Context, p Policy, session, thread string) error {
	if err := g.verifyAccountBinding(ctx, p); err != nil {
		return err
	}
	var known bool
	if err := g.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM native_thread_bindings WHERE session_id=? AND thread_id=? AND account_key=?)", session, thread, p.AccountKey).Scan(&known); err != nil {
		return err
	}
	if !known {
		return deny(ErrPolicy, "native thread is not registered to this governed host")
	}
	return nil
}

// VerifyNativeRecovery authorizes observation of an existing unsettled turn,
// never a new dispatch. A historical registry entry alone does not prove that
// this host owns the active or uncertain attempt on the native thread.
func (g *Gate) VerifyNativeRecovery(ctx context.Context, session, thread string) error {
	p, err := g.current()
	if err != nil {
		return err
	}
	if err := g.verifyNativeThread(ctx, p, session, thread); err != nil {
		return err
	}
	var bound bool
	if err := g.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attempts a JOIN native_attempt_threads n ON n.attempt_id=a.id WHERE a.session_id=? AND n.thread_id=? AND a.account_key=? AND n.account_key=? AND a.dispatched_at<>'' AND a.provider_turn_id<>'' AND a.state IN ('active','uncertain'))", session, thread, p.AccountKey, p.AccountKey).Scan(&bound); err != nil {
		return err
	}
	if !bound {
		return deny(ErrUncertain, "native recovery requires this host's exact bound unsettled turn")
	}
	return nil
}

func (g *Gate) NativeThreads(ctx context.Context) ([]NativeThreadBinding, error) {
	p, err := g.current()
	if err != nil {
		return nil, err
	}
	rows, err := g.db.QueryContext(ctx, "SELECT session_id,thread_id,account_key FROM native_thread_bindings WHERE account_key=? ORDER BY thread_id,session_id", p.AccountKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NativeThreadBinding{}
	for rows.Next() {
		var b NativeThreadBinding
		if err := rows.Scan(&b.Session, &b.Thread, &b.AccountKey); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveReceiptSession derives ownership from an exact admitted native root turn,
// never from whichever AO session happens to own a transcript file today.
func (g *Gate) ResolveReceiptSession(ctx context.Context, thread, turn string) (string, error) {
	if thread == "" || turn == "" {
		return "", ErrUnboundNativeTurn
	}
	p, err := g.current()
	if err != nil {
		return "", err
	}
	if err := g.verifyAccountBinding(ctx, p); err != nil {
		return "", err
	}
	rows, err := g.db.QueryContext(ctx, "SELECT DISTINCT a.session_id FROM attempts a JOIN native_attempt_threads n ON n.attempt_id=a.id WHERE n.thread_id=? AND a.provider_turn_id=? AND a.dispatched_at<>'' AND a.account_key=? AND n.account_key=? LIMIT 2", thread, turn, p.AccountKey, p.AccountKey)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return "", err
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(owners) != 1 {
		return "", ErrUnboundNativeTurn
	}
	return owners[0], nil
}
