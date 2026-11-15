package agent

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Owner-local qualification: every dependency here is a fake. No provider
// executable, credential check, HTTP request, or model turn is performed.
func TestLifecycleSandboxAuthentication(t *testing.T) {
	for _, env := range []string{"1", "", "true"} {
		t.Run("flag="+env, func(t *testing.T) {
			t.Setenv("AO_LOCAL_LIFECYCLE_TEST", env)
			for _, accountChecker := range []bool{false, true} {
				name := "adapter"
				if accountChecker {
					name = "account"
				}
				for _, entrypoint := range []string{"display", "launch", "force", "warm", "recheck"} {
					t.Run(name+"/"+entrypoint, func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						installed := make(chan struct{}, 1)
						fake := &readinessTestAgent{
							resolve: func(context.Context) (string, error) {
								select {
								case installed <- struct{}{}:
								default:
								}
								return "/fixture/agent", nil
							},
							auth: func(context.Context) (ports.AgentAuthStatus, error) {
								return ports.AgentAuthStatusAuthorized, nil
							},
						}
						var accountCalls atomic.Int32
						cfg := readinessCoordinatorConfig{
							Agents:  []agentregistry.HarnessAgent{readinessHarness("fixture", "Fixture", fake)},
							Context: ctx, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
						}
						if accountChecker {
							cfg.AuthenticationCheck = func(context.Context, string, domain.AgentReadinessPurpose) (domain.AgentAuthenticationObservation, bool) {
								accountCalls.Add(1)
								return successfulAuthentication(time.Now(), domain.AgentAuthenticationAuthorized, domain.AgentReadinessReasonAuthorized, "Fixture authorized"), true
							}
						}
						coordinator := newReadinessCoordinator(cfg)
						var got []domain.AgentReadinessSnapshot
						var err error
						switch entrypoint {
						case "display":
							got, err = coordinator.Ensure(ctx, nil, domain.AgentReadinessPurposeDisplay)
						case "launch":
							got, err = coordinator.Ensure(ctx, nil, domain.AgentReadinessPurposeLaunch)
						case "force":
							got, err = coordinator.Force(ctx, nil, domain.AgentReadinessPurposeDisplay)
						case "warm":
							select {
							case <-coordinator.Warm():
								got = coordinator.Snapshot()
							case <-ctx.Done():
								t.Fatal(ctx.Err())
							}
						case "recheck":
							svc := &Service{readiness: coordinator, logger: cfg.Logger}
							svc.RecheckAgent("fixture")
							select {
							case <-installed:
							case <-ctx.Done():
								t.Fatal(ctx.Err())
							}
							// Join the asynchronous recheck before inspecting its result.
							got, err = coordinator.Ensure(ctx, nil, domain.AgentReadinessPurposeDisplay)
						}
						if err != nil || len(got) != 1 {
							t.Fatalf("readiness = %v, %v", got, err)
						}
						if got[0].Installation.State != domain.AgentInstallationInstalled || fake.resolveCalls.Load() == 0 {
							t.Fatalf("installation was not checked: %#v", got[0].Installation)
						}
						if env == "1" {
							if fake.authCalls.Load() != 0 || accountCalls.Load() != 0 {
								t.Fatalf("sandbox performed authentication: adapter=%d account=%d", fake.authCalls.Load(), accountCalls.Load())
							}
							auth := got[0].Authentication
							if auth.State != domain.AgentAuthenticationUnknown || auth.ReasonCode != domain.AgentReadinessReasonNotChecked || auth.CheckedAt != nil || got[0].EffectiveReadiness == domain.AgentReadinessReady {
								t.Fatalf("sandbox reported checked/ready authentication: %#v", got[0])
							}
						} else {
							if got[0].Authentication.State != domain.AgentAuthenticationAuthorized || got[0].EffectiveReadiness != domain.AgentReadinessReady {
								t.Fatalf("normal authentication changed: %#v", got[0])
							}
							if accountChecker && (accountCalls.Load() != 1 || fake.authCalls.Load() != 0) || !accountChecker && (accountCalls.Load() != 0 || fake.authCalls.Load() != 1) {
								t.Fatalf("normal authentication calls: adapter=%d account=%d", fake.authCalls.Load(), accountCalls.Load())
							}
						}
					})
				}
			}
		})
	}
}
