package inferenceguard

import (
	"context"
	"errors"
)

// ErrUnboundReceipt keeps historical or foreign native logs from being charged
// to the currently verified account. Explicit offline forensic exports remain separate.
var ErrUnboundReceipt = errors.New("native receipt has no verified dispatch binding")

// IngestBoundReceipt accepts only a root turn this authority actually dispatched
// after native account verification. Unknown historical account identity is not guessed.
func (g *Gate) IngestBoundReceipt(ctx context.Context, r UsageReceipt) error {
	if r.NativeThreadID == "" {
		return ErrUnboundReceipt
	}
	{
		session, err := g.ResolveReceiptSession(ctx, r.NativeThreadID, r.ProviderTurnID)
		if errors.Is(err, ErrUnboundNativeTurn) {
			return ErrUnboundReceipt
		}
		if err != nil {
			return err
		}
		r.Session = session
	}
	p, err := g.current()
	if err != nil {
		return err
	}
	if p.NativeAccountSHA256 == "" {
		return ErrUnboundReceipt
	}
	var known bool
	if err := g.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM attempts a JOIN account_binding b ON b.id=1 WHERE a.session_id=? AND a.provider_turn_id=? AND a.dispatched_at<>'' AND a.account_key=? AND b.native_sha256=?)",
		r.Session, r.ProviderTurnID, p.AccountKey, p.NativeAccountSHA256).Scan(&known); err != nil {
		return err
	}
	if !known {
		return ErrUnboundReceipt
	}
	if err := g.IngestReceipt(ctx, r); err != nil {
		return err
	}
	// Preserve actual usage even when it proves the native model violated policy.
	// The durable alarm stops further dispatch and cannot be cleared as a quota reset.
	if err := g.ObserveNativeSelection(ctx, r.Session, r.ProviderTurnID, r.Model, r.Effort); err != nil && !errors.Is(err, ErrPolicy) {
		return err
	}
	return nil
}
