package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemonCleanupCancelsBeforeOrderedIdempotentDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx, workers := withDaemonWorkers(ctx)
	exited := make(chan struct{})
	startDaemonWorker(ctx, func() { <-ctx.Done(); close(exited) })
	var order []int
	cleanup := daemonCleanup{cancel: cancel}
	cleanup.add(20, func() { order = append(order, 20) })
	cleanup.add(10, func() {
		if ctx.Err() == nil {
			t.Error("cleanup ran before cancellation")
		}
		workers.wait()
		select {
		case <-exited:
		default:
			t.Error("worker not drained")
		}
		order = append(order, 10)
	})
	cleanup.run()
	cleanup.run()
	if len(order) != 2 || order[0] != 10 || order[1] != 20 {
		t.Fatalf("cleanup order=%v", order)
	}
	var late atomic.Int64
	startDaemonWorker(ctx, func() { late.Add(1) })
	time.Sleep(time.Millisecond)
	if late.Load() != 0 {
		t.Fatal("late worker started after drain")
	}
}
func TestDaemonWorkerCompletionClosesWhenCancellationRefusesStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx, workers := withDaemonWorkers(ctx)
	cancel()
	workers.wait()
	done := make(chan struct{})
	var calls atomic.Int64
	startDaemonWorkerWithDone(ctx, done, func() { calls.Add(1) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refused worker stranded completion")
	}
	if calls.Load() != 0 {
		t.Fatal("refused worker executed")
	}
}
