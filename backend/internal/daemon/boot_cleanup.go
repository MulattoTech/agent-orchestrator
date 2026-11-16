package daemon

import (
	"context"
	"sort"
	"sync"
)

// daemonCleanup owns each shutdown operation exactly once, including failures
// between constructors. Cancellation always precedes waiting; services close
// before the CDC/store owners beneath them.
type daemonCleanup struct {
	once   sync.Once
	cancel context.CancelFunc
	stages []daemonCleanupStage
}
type daemonCleanupStage struct {
	order int
	close func()
}

func (c *daemonCleanup) add(order int, close func()) {
	c.stages = append(c.stages, daemonCleanupStage{order, close})
}
func (c *daemonCleanup) run() {
	c.once.Do(func() {
		c.cancel()
		sort.SliceStable(c.stages, func(i, j int) bool { return c.stages[i].order < c.stages[j].order })
		for _, stage := range c.stages {
			stage.close()
		}
	})
}

// daemonWorkers tracks daemon-owned background tasks that formerly relied only
// on cancellation. The private context key carries ownership through existing
// constructor signatures without adding any externally selectable mode.
type daemonWorkerKey struct{}
type daemonWorkers struct {
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func withDaemonWorkers(ctx context.Context) (context.Context, *daemonWorkers) {
	workers := &daemonWorkers{}
	return context.WithValue(ctx, daemonWorkerKey{}, workers), workers
}
func startDaemonWorker(ctx context.Context, run func()) bool {
	if workers, ok := ctx.Value(daemonWorkerKey{}).(*daemonWorkers); ok {
		workers.mu.Lock()
		if workers.closing || ctx.Err() != nil {
			workers.mu.Unlock()
			return false
		}
		workers.wg.Add(1)
		workers.mu.Unlock()
		go func() { defer workers.wg.Done(); run() }()
		return true
	}
	go run()
	return true
}
func (w *daemonWorkers) wait() {
	w.mu.Lock()
	w.closing = true
	w.mu.Unlock()
	w.wg.Wait()
}
func startDaemonWorkerWithDone(ctx context.Context, done chan struct{}, run func()) {
	if !startDaemonWorker(ctx, func() { defer close(done); run() }) {
		close(done)
	}
}
