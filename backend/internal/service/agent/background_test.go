package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type blockingWarmerAgent struct {
	fakeAuthAgent
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingWarmerAgent) ResolveBinaryPresence(ctx context.Context) (string, error) {
	a.once.Do(func() { close(a.entered) })
	<-a.release // Deliberately remain alive after cancellation to test truthful drain.
	return "", ctx.Err()
}

type blockingWarmerDiscoverer struct {
	*fakeModelDiscoverer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingWarmerDiscoverer) Discover(ctx context.Context, request ports.AgentModelDiscoveryRequest) (ports.AgentModelCatalog, error) {
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return ports.AgentModelCatalog{}, ctx.Err()
}

type blockingWarmerGate struct {
	ports.CodexOperationGate
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *blockingWarmerGate) AcquireExclusive(ctx context.Context) (ports.CodexOperationLease, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return nil, ctx.Err()
}

func assertWarmerDrain(t *testing.T, service *Service, cancel context.CancelFunc, entered, release chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fake warmer never started")
	}
	cancel()
	short, shortCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer shortCancel()
	if err := service.WaitWarmers(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("undrained nested worker reported complete: %v", err)
	}
	close(release)
	drain, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := service.WaitWarmers(drain); err != nil {
		t.Fatalf("released worker failed to drain: %v", err)
	}
	service.background.mu.Lock()
	active := service.background.active
	service.background.mu.Unlock()
	if active != 0 {
		t.Fatalf("drain returned with %d workers", active)
	}
}

func TestAgentWarmersDrainNestedReadiness(t *testing.T) {
	t.Setenv("AO_LOCAL_LIFECYCLE_TEST", "")
	deps := explicitTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.Context = ctx
	fake := &blockingWarmerAgent{fakeAuthAgent: fakeAuthAgent{status: ports.AgentAuthStatusAuthorized}, entered: make(chan struct{}), release: make(chan struct{})}
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent { return explicitTestHarness(fake) })
	if err != nil {
		t.Fatal(err)
	}
	service.WarmReadiness()
	assertWarmerDrain(t, service, cancel, fake.entered, fake.release)
}

func TestAgentWarmersDrainNestedCatalogLoad(t *testing.T) {
	t.Setenv("AO_LOCAL_LIFECYCLE_TEST", "")
	deps := explicitTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.Context = ctx
	fake := &blockingWarmerDiscoverer{fakeModelDiscoverer: successfulModelDiscoverer(), entered: make(chan struct{}), release: make(chan struct{})}
	deps.Discoverer = fake
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent {
		return explicitTestHarness(fakeAuthAgent{status: ports.AgentAuthStatusAuthorized})
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.WarmReadiness():
	case <-time.After(5 * time.Second):
		t.Fatal("fake readiness timeout")
	}
	service.WarmModelCatalogs(ctx)
	assertWarmerDrain(t, service, cancel, fake.entered, fake.release)
}

func TestAgentWarmersDrainNestedAccountReconciliation(t *testing.T) {
	deps := explicitTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.Context = ctx
	fake := &blockingWarmerGate{entered: make(chan struct{}), release: make(chan struct{})}
	deps.CodexOperationGate = fake
	service, err := NewWithExplicitDeps(deps, func() []agentregistry.HarnessAgent {
		return explicitTestHarness(fakeAuthAgent{status: ports.AgentAuthStatusAuthorized})
	})
	if err != nil {
		t.Fatal(err)
	}
	// This unit test isolates reconciliation lifetime. Native credential-store
	// ACL validation is retained and qualified separately; no credential is made.
	service.codexAccounts.accountStoreReady = true
	service.WarmCodexAccounts()
	assertWarmerDrain(t, service, cancel, fake.entered, fake.release)
}

func TestAgentWarmersCancelLongRetryAndAwaitNestedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := newService(nil, nil, nil)
	service.ctx = ctx
	entered, release := make(chan struct{}), make(chan struct{})
	var timerRan atomic.Bool
	service.background.After(ctx, time.Hour, func() { timerRan.Store(true) })
	service.background.Go(func() {
		service.background.Go(func() { close(entered); <-release })
	})
	assertWarmerDrain(t, service, cancel, entered, release)
	if timerRan.Load() {
		t.Fatal("canceled retry callback executed")
	}
}

func TestAgentWarmersRejectDrainBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := newService(nil, nil, nil)
	service.ctx = ctx
	if err := service.WaitWarmers(context.Background()); err == nil {
		t.Fatal("uncanceled service accepted as drained")
	}
	cancel()
	if err := service.WaitWarmers(context.Background()); err != nil {
		t.Fatal(err)
	}
}
