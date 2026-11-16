package inferenceguard

import "context"

// MarkUncertain retains an admitted lease after an unknown acknowledgement.
// A generic RPC/internal failure cannot prove the provider did not accept work.
func (g *Gate) MarkUncertain(ctx context.Context, id, reason string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.ExecContext(ctx, "UPDATE attempts SET state='uncertain',error_class=? WHERE id=? AND state IN ('active','uncertain')", reason, id)
	return err
}
