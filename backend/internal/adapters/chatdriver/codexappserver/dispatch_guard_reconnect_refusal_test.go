package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func changeRecoveryPolicy(t *testing.T, change func(*inferenceguard.Policy)) {
	t.Helper()
	policy, err := inferenceguard.ReadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	change(&policy)
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(inferenceguard.PolicyEnv), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInferenceGuardReconnectCapacityExceptionStaysBounded(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*testing.T, *guardedReconnectFixture, *ports.ChatResumeConfig)
		want      error
		hostCalls int
		operation string
	}{
		{
			name: "different_registered_host", want: inferenceguard.ErrUncertain,
			configure: func(t *testing.T, f *guardedReconnectFixture, cfg *ports.ChatResumeConfig) {
				cfg.SessionID = "historical-other-host"
				if err := f.guard.RegisterNativeThread(context.Background(), string(cfg.SessionID), cfg.ProviderConversationID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different_registered_thread", want: inferenceguard.ErrUncertain,
			configure: func(t *testing.T, f *guardedReconnectFixture, cfg *ports.ChatResumeConfig) {
				cfg.ProviderConversationID = "unrelated-thread"
				if err := f.guard.RegisterNativeThread(context.Background(), string(cfg.SessionID), cfg.ProviderConversationID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unbound_uncertain_request", want: inferenceguard.ErrUncertain,
			configure: func(t *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				ctx := context.Background()
				if err := f.guard.Complete(ctx, string(f.start.SessionID), "retained-turn", ""); err != nil {
					t.Fatal(err)
				}
				selection, err := f.guard.Resolve("implementation", string(f.start.SessionID), "gpt-test", "medium")
				if err != nil {
					t.Fatal(err)
				}
				request := inferenceguard.Request{
					NativeThreadID: "thread-1", ID: "unacknowledged", Session: string(f.start.SessionID),
					Project: string(f.start.ProjectID), Operation: "turn/start", Selection: selection,
				}
				if err := f.guard.Dispatch(ctx, request, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				if err := f.guard.MarkUncertain(ctx, request.ID, "synthetic lost acknowledgement"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing_host", want: ports.ErrChatHostNotRunning, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				f.driver.connectHost = func(context.Context, persistenthost.Config) (*persistenthost.Transport, error) {
					f.hostCalls++
					return nil, persistenthost.ErrNotRunning
				}
			},
		},
		{
			name: "host_ownership_inconclusive", want: ports.ErrChatRecoveryInconclusive, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				f.driver.connectHost = func(context.Context, persistenthost.Config) (*persistenthost.Transport, error) {
					f.hostCalls++
					return nil, persistenthost.ErrOwnershipInconclusive
				}
			},
		},
		{
			name: "non_live_transport", want: ports.ErrChatRecoveryInconclusive, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				connect := f.driver.connectHost
				f.driver.connectHost = func(ctx context.Context, cfg persistenthost.Config) (*persistenthost.Transport, error) {
					transport, err := connect(ctx, cfg)
					if transport != nil {
						transport.Reconnected = false
					}
					return transport, err
				}
			},
		},
		{
			name: "uninitialized_host", want: inferenceguard.ErrPolicy, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				f.server.replyError("account/read", -32600, "Not initialized")
			},
		},
		{
			name: "foreign_native_account", want: inferenceguard.ErrPolicy, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				f.server.reply("account/read", "{\"account\":{\"type\":\"chatgpt\",\"email\":\"other@example.invalid\"}}")
			},
		},
		{
			name: "unsupported_native_catalog", want: inferenceguard.ErrPolicy, hostCalls: 1,
			configure: func(_ *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				f.server.reply("model/list", "{\"data\":[]}")
			},
		},
		{
			name: "owner_soft_pause", want: inferenceguard.ErrQuota,
			configure: func(t *testing.T, _ *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				changeRecoveryPolicy(t, func(p *inferenceguard.Policy) { p.Pause = true })
			},
		},
		{
			name: "owner_emergency_stop", want: inferenceguard.ErrQuota,
			configure: func(t *testing.T, _ *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				changeRecoveryPolicy(t, func(p *inferenceguard.Policy) { p.EmergencyStop = true })
			},
		},
		{
			name: "quota_lockout", want: inferenceguard.ErrQuota,
			configure: func(t *testing.T, f *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				if err := f.guard.Trip(context.Background(), time.Time{}, "synthetic quota stop"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "budget_exhausted", want: inferenceguard.ErrQuota,
			configure: func(t *testing.T, _ *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				changeRecoveryPolicy(t, func(p *inferenceguard.Policy) { p.TurnBudget = 1 })
			},
		},
		{
			name: "policy_selection_changed", want: inferenceguard.ErrPolicy,
			configure: func(t *testing.T, _ *guardedReconnectFixture, _ *ports.ChatResumeConfig) {
				changeRecoveryPolicy(t, func(p *inferenceguard.Policy) {
					p.Roles["implementation"] = inferenceguard.RolePolicy{DefaultModel: "different", Models: []string{"different"}}
				})
			},
		},
		{name: "ordinary_resume", want: inferenceguard.ErrCapacity, operation: "resume"},
		{name: "new_start", want: inferenceguard.ErrCapacity, operation: "start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardedReconnectFixture(t, true)
			cfg := f.resumeConfig()
			// A failed observation must never use this flag to create a replacement
			// thread, even when a caller supplied fresh-if-missing consent.
			cfg.FreshIfMissing = true
			if tc.configure != nil {
				tc.configure(t, f, &cfg)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			before, err := f.guard.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var conv ports.ChatConversation
			switch tc.operation {
			case "resume":
				conv, err = f.driver.Resume(ctx, cfg)
			case "start":
				conv, err = f.driver.Start(ctx, f.start)
			default:
				conv, err = f.driver.Reconnect(ctx, cfg)
			}
			if conv != nil {
				_ = conv.Close()
				t.Fatal("refused recovery returned a conversation")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("recovery refusal = %v, want %v", err, tc.want)
			}
			after, snapshotErr := f.guard.Snapshot(ctx)
			if snapshotErr != nil || after.Active != before.Active || after.Uncertain != before.Uncertain || after.Completed != before.Completed || after.BudgetUsed != before.BudgetUsed {
				t.Fatalf("refusal changed durable outcome: before=%+v after=%+v error=%v", before, after, snapshotErr)
			}
			if f.hostCalls != tc.hostCalls || f.prepareCalls != 0 || f.launchCalls != 0 {
				t.Fatalf("recovery side effects: host=%d want=%d prepare=%d launch=%d", f.hostCalls, tc.hostCalls, f.prepareCalls, f.launchCalls)
			}
			for _, method := range []string{"initialize", "initialized", "thread/start", "thread/resume", "turn/start"} {
				if f.server.sentMethod(method) {
					t.Fatalf("refused observation sent forbidden method %s", method)
				}
			}
		})
	}
}
