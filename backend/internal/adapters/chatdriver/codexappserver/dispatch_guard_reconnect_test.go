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

type guardedReconnectFixture struct {
	driver       *Driver
	server       *scriptedServer
	start        ports.ChatStartConfig
	guard        *inferenceguard.Gate
	hostCalls    int
	prepareCalls int
	launchCalls  int
}

func newGuardedReconnectFixture(t *testing.T, restart bool) *guardedReconnectFixture {
	t.Helper()
	d, srv, cfg := guardedDriver(t)
	policy, err := inferenceguard.ReadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.MaximumInference = 1
	policy.MaximumPerProject = 1
	policy.MaximumReviewers = 1
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(inferenceguard.PolicyEnv), data, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := inferenceguard.ForDataDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := g.VerifyAccount(ctx, policy.NativeAccountSHA256); err != nil {
		t.Fatal(err)
	}
	if err := g.RegisterNativeThread(ctx, string(cfg.SessionID), "thread-1"); err != nil {
		t.Fatal(err)
	}
	selection, err := g.Resolve("implementation", string(cfg.SessionID), "gpt-test", "medium")
	if err != nil {
		t.Fatal(err)
	}
	request := inferenceguard.Request{
		NativeThreadID: "thread-1", ID: "retained-request", Session: string(cfg.SessionID),
		Project: string(cfg.ProjectID), Operation: "turn/start", Selection: selection,
	}
	if err := g.Dispatch(ctx, request, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, request.ID, "retained-turn"); err != nil {
		t.Fatal(err)
	}
	if restart {
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		g, err = inferenceguard.ForDataDir(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = g.Close() })
	}

	// newTestDriver's injected spawn only returns io.Pipe endpoints. No native
	// host, process, socket, credential lookup or provider command is invoked.
	proc, err := d.spawn(ctx, "synthetic-only", cfg.WorkspacePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.stdin.Close() })
	f := &guardedReconnectFixture{driver: d, server: srv, start: cfg, guard: g}
	d.spawn = func(context.Context, string, string, []string) (*process, error) {
		f.launchCalls++
		return nil, errors.New("fixture forbids a provider launch")
	}
	d.persistent = true
	d.connectHost = func(_ context.Context, config persistenthost.Config) (*persistenthost.Transport, error) {
		f.hostCalls++
		if !config.ReconnectOnly {
			t.Fatal("recovery allowed a new provider host")
		}
		return &persistenthost.Transport{
			Stdin: proc.stdin, Stdout: proc.stdout, Reconnected: true, NextRequestID: 41,
		}, nil
	}
	srv.replyError("initialize", -32600, "Already initialized")
	return f
}

func (f *guardedReconnectFixture) resumeConfig() ports.ChatResumeConfig {
	return ports.ChatResumeConfig{
		SessionID: f.start.SessionID, ProjectID: f.start.ProjectID, PolicyRole: f.start.PolicyRole,
		DataDir: f.start.DataDir, WorkspacePath: f.start.WorkspacePath,
		ProviderConversationID: "thread-1", Model: "gpt-test", Effort: "medium",
		PrepareEnv: func(context.Context) (map[string]string, error) {
			f.prepareCalls++
			return nil, errors.New("fixture forbids preparing a provider launch")
		},
	}
}

func TestInferenceGuardReconnectAtCapacityRecoversKnownTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "active"},
		{name: "restart_uncertain", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardedReconnectFixture(t, tc.restart)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			before, err := f.guard.Snapshot(ctx)
			if err != nil || before.Active+before.Uncertain != 1 || before.BudgetUsed != 1 {
				t.Fatalf("seeded retained turn: %+v %v", before, err)
			}
			if tc.restart && before.Uncertain != 1 {
				t.Fatalf("restart did not preserve uncertain lease: %+v", before)
			}
			conv, err := f.driver.Reconnect(ctx, f.resumeConfig())
			if err != nil {
				after, snapshotErr := f.guard.Snapshot(ctx)
				t.Fatalf("known bound turn could not reconnect: %v; host_calls=%d active=%d uncertain=%d completed=%d snapshot_error=%v",
					err, f.hostCalls, after.Active, after.Uncertain, after.Completed, snapshotErr)
			}
			defer conv.Close()
			if !conv.(*conversation).ReconnectedLive() {
				t.Fatal("recovery did not attach to the initialized surviving host")
			}
			afterAttach, err := f.guard.Snapshot(ctx)
			if err != nil || afterAttach.Active != before.Active || afterAttach.Uncertain != before.Uncertain || afterAttach.Completed != 0 {
				t.Fatalf("attachment changed outcome without completion evidence: %+v %v", afterAttach, err)
			}
			if _, err := conv.SendTurn(ctx, ports.ChatUserMessage{Text: "must remain queued", ClientMessageID: "different-request"}); !errors.Is(err, inferenceguard.ErrCapacity) {
				t.Fatalf("recovery bypassed new-turn capacity: %v", err)
			}
			f.server.push("{\"method\":\"turn/completed\",\"params\":{\"threadId\":\"thread-1\",\"turn\":{\"id\":\"retained-turn\",\"status\":\"completed\",\"items\":[]}}}")
			nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted)
			afterCompletion, err := f.guard.Snapshot(ctx)
			if err != nil || afterCompletion.Active != 0 || afterCompletion.Uncertain != 0 || afterCompletion.Completed != 1 || afterCompletion.BudgetUsed != 1 {
				t.Fatalf("exact completion did not settle retained turn: %+v %v", afterCompletion, err)
			}
			if f.hostCalls != 1 || f.prepareCalls != 0 || f.launchCalls != 0 {
				t.Fatalf("recovery side effects: host=%d prepare=%d launch=%d", f.hostCalls, f.prepareCalls, f.launchCalls)
			}
			for _, method := range []string{"thread/start", "thread/resume", "turn/start"} {
				if f.server.sentMethod(method) {
					t.Fatalf("recovery sent forbidden method %s", method)
				}
			}
		})
	}
}
