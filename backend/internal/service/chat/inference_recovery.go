package chat

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

// resumeGuardedQueues is a bounded, local observer. It never wakes or creates a
// provider controller. A paused/exhausted authority is checked before touching
// queues, and the actual native boundary rechecks before every inference write.
func (s *Service) resumeGuardedQueues(ctx context.Context, gate *inferenceguard.Gate) error {
	s.mu.RLock()
	controllers := make([]*Controller, 0, len(s.controllers))
	for _, c := range s.controllers {
		controllers = append(controllers, c)
	}
	s.mu.RUnlock()
	sort.Slice(controllers, func(i, j int) bool { return controllers[i].sessionID < controllers[j].sessionID })
	for _, c := range controllers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := gate.Check(ctx); err != nil {
			if inferenceguard.PreDispatchRefusal(err) {
				return nil
			}
			return err
		}
		if !c.sendMu.TryLock() {
			continue
		}
		preview, err := c.store.ListQueuedBatch(ctx, c.conversation.ID, 1)
		if err == nil && len(preview) > 0 {
			turn, readErr := c.store.TurnByID(ctx, preview[0].TurnID)
			if readErr != nil {
				err = readErr
			} else if strings.HasPrefix(turn.ErrorMessage, inferenceguard.ErrPolicy.Error()) {
				// A model-policy correction requires explicit owner action, not a
				// perpetual metadata/retry loop against an unsupported provider.
				c.sendMu.Unlock()
				continue
			}
		}
		if err == nil {
			err = c.drainLockedWithLimit(ctx, true, 1)
		}
		c.sendMu.Unlock()
		if err != nil && !errors.Is(err, domain.ErrNoQueuedTurn) && !inferenceguard.PreDispatchRefusal(err) {
			return err
		}
	}
	return nil
}
