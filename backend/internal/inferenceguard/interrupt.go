package inferenceguard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RegisterStopper registers one known governed controller. Call the returned
// function when that controller detaches. It grants no process-kill authority.
func (g *Gate) RegisterStopper(stop func(context.Context) error) func() {
	if stop == nil {
		return func() {}
	}
	g.stopMu.Lock()
	g.stopNext++
	id := g.stopNext
	if g.stoppers == nil {
		g.stoppers = map[uint64]func(context.Context) error{}
	}
	g.stoppers[id] = stop
	g.stopMu.Unlock()
	return func() { g.stopMu.Lock(); delete(g.stoppers, id); g.stopMu.Unlock() }
}

// InterruptActive requests bounded provider interrupts for known controllers.
// It cannot prove that native work stopped; exact completion still releases leases.
func (g *Gate) InterruptActive(ctx context.Context) error {
	if !g.stopping.CompareAndSwap(false, true) {
		return nil
	}
	defer g.stopping.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	g.stopMu.Lock()
	stoppers := make([]func(context.Context) error, 0, len(g.stoppers))
	for _, stop := range g.stoppers {
		stoppers = append(stoppers, stop)
	}
	g.stopMu.Unlock()
	results := make(chan error, len(stoppers))
	var wg sync.WaitGroup
	for _, stop := range stoppers {
		wg.Go(func() { results <- stop(ctx) })
	}
	go func() { wg.Wait(); close(results) }()
	var errs []error
	for {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, fmt.Errorf("native interrupt acknowledgement incomplete: %w", ctx.Err()))...)
		case err, ok := <-results:
			if !ok {
				return errors.Join(errs...)
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
}

// HardStopped distinguishes emergency/exhaustion from a soft admission pause.
func (g *Gate) HardStopped(ctx context.Context) (bool, error) {
	if g.blocked.Load() {
		return true, nil
	}
	p, err := g.current()
	if err != nil {
		return true, err
	}
	if p.EmergencyStop {
		return true, nil
	}
	var status string
	err = g.db.QueryRowContext(ctx, "SELECT status FROM authority WHERE id=1").Scan(&status)
	return err != nil || status == "EXHAUSTED", err
}
