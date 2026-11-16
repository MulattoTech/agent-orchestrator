package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// backgroundWork tracks the complete tree of work started by the catalog,
// readiness and account services. A parent is registered before it is launched,
// so nested work cannot briefly make the group appear drained.
type backgroundWork struct {
	mu     sync.Mutex
	active int
	done   chan struct{}
}

func (w *backgroundWork) Go(run func()) {
	// Private component tests may construct a component without the shared
	// service lifetime. Production and explicit constructors always provide it.
	if w == nil {
		go run()
		return
	}
	w.mu.Lock()
	if w.active == 0 {
		w.done = make(chan struct{})
	}
	w.active++
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			w.active--
			if w.active == 0 {
				close(w.done)
			}
			w.mu.Unlock()
		}()
		run()
	}()
}

func (w *backgroundWork) After(ctx context.Context, delay time.Duration, run func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() == nil {
				run()
			}
		}
	})
}

func (w *backgroundWork) wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	for {
		w.mu.Lock()
		active, done := w.active, w.done
		w.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitWarmers drains readiness, model-catalog and account background work,
// including shared singleflight calls and retry timers started by warmers.
// The caller must stop incoming requests and cancel the service's construction
// context first. A timeout reports incomplete shutdown; it never declares an
// uncooperative adapter drained or closes storage underneath it.
// Account-switch transaction workers retain their separate Wait contract.
func (s *Service) WaitWarmers(ctx context.Context) error {
	if s.ctx == nil || s.ctx.Err() == nil {
		return errors.New("cancel agent service context before draining background work")
	}
	return s.background.wait(ctx)
}
