package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func guardPolicy(t *testing.T) inferenceguard.Policy {
	t.Helper()
	p := inferenceguard.Policy{Version: 1, AccountKey: "scripted-account", AuthorizationEpoch: "offline-test", MaximumEffort: "medium", MaximumInference: 2, MaximumPerProject: 2, MaximumReviewers: 1, TurnBudget: 20, Roles: map[string]inferenceguard.RolePolicy{}}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		p.Roles[role] = inferenceguard.RolePolicy{DefaultModel: "gpt-test", Models: []string{"gpt-test"}}
	}
	p.Roles["lightweight"] = inferenceguard.RolePolicy{DefaultModel: "gpt-test", Models: []string{"gpt-test", "luna"}}
	p.AuthorityPath = filepath.Join(t.TempDir(), "shared-authority.db")
	p.NativeAccountSHA256 = inferenceguard.AccountFingerprint("chatgpt", "fake@example.invalid")
	path := filepath.Join(t.TempDir(), "policy.json")
	data, _ := json.Marshal(p)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(inferenceguard.PolicyEnv, path)
	return p
}
func guardedDriver(t *testing.T) (*Driver, *scriptedServer, ports.ChatStartConfig) {
	t.Helper()
	guardPolicy(t)
	d, srv := newTestDriver(t)
	srv.reply("account/rateLimits/read", "{}")
	srv.reply("account/read", `{"account":{"type":"chatgpt","email":"fake@example.invalid"},"requiresOpenaiAuth":true}`)
	srv.reply("model/list", `{"data":[{"id":"gpt-test","model":"gpt-test","displayName":"GPT Test","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"}]}]}`)
	srv.reply("thread/start", `{"thread":{"id":"thread-1"},"model":"gpt-test","reasoningEffort":"medium"}`)
	cfg := ports.ChatStartConfig{SessionID: "test-session", ProjectID: "project", PolicyRole: "implementation", DataDir: t.TempDir(), WorkspacePath: t.TempDir(), Model: "gpt-test", Effort: "medium"}
	g, err := inferenceguard.ForDataDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return d, srv, cfg
}
func TestInferenceGuardAllRoleRequestsRefuseHighBeforeTurnWrite(t *testing.T) {
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		t.Run(role, func(t *testing.T) {
			d, srv, cfg := guardedDriver(t)
			cfg.PolicyRole = role
			conv, err := d.Start(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conv.Close()
			_, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "offline fake", Settings: ports.ChatTurnSettings{Model: "gpt-test", Effort: "high"}})
			if !errors.Is(err, inferenceguard.ErrPolicy) || srv.sentMethod("turn/start") {
				t.Fatalf("role %s error %v dispatched=%v", role, err, srv.sentMethod("turn/start"))
			}
		})
	}
}
func TestInferenceGuardStartResumeImportTakeoverAndReviewerCannotBypass(t *testing.T) {
	for _, path := range []string{"new", "restore", "import", "takeover", "reviewer", "fork"} {
		t.Run(path, func(t *testing.T) {
			d, srv, cfg := guardedDriver(t)
			cfg.Effort = "high"
			var err error
			switch path {
			case "new":
				_, err = d.Start(context.Background(), cfg)
			case "reviewer":
				cfg.PolicyRole = "reviewer"
				cfg.Model = "luna"
				cfg.ReadOnly = true
				_, err = d.Start(context.Background(), cfg)
			case "fork":
				_, err = d.ForkIntoHost(context.Background(), "parent", "turn", cfg)
			default:
				_, err = d.Resume(context.Background(), ports.ChatResumeConfig{SessionID: cfg.SessionID, PolicyRole: cfg.PolicyRole, DataDir: cfg.DataDir, WorkspacePath: cfg.WorkspacePath, ProviderConversationID: "stored-thread", Model: cfg.Model, Effort: cfg.Effort, ReconnectOnly: path == "takeover"})
			}
			if !errors.Is(err, inferenceguard.ErrPolicy) {
				t.Fatal(err)
			}
			if srv.sentMethod("initialize") || srv.sentMethod("turn/start") {
				t.Fatal("policy violation reached provider process")
			}
		})
	}
}
func TestInferenceGuardFinalWireContainsExplicitResolvedMedium(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	cfg.Model = ""
	cfg.Effort = ""
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	if _, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "fake only", ClientMessageID: "once"}); err != nil {
		t.Fatal(err)
	}
	frame := srv.awaitFrame(func(f frame) bool { return f.Method == "turn/start" })
	var params map[string]any
	if err = json.Unmarshal(frame.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params["model"] != "gpt-test" || params["effort"] != "medium" {
		t.Fatalf("wire=%v", params)
	}
	if _, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "fake only", ClientMessageID: "once"}); !errors.Is(err, inferenceguard.ErrUncertain) {
		t.Fatal("duplicate request not fenced", err)
	}
}
func TestInferenceGuardUnsupportedCatalogModelFailsClosed(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	srv.reply("model/list", `{"data":[{"id":"different","model":"different","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}`)
	_, err := d.Start(context.Background(), cfg)
	if !errors.Is(err, inferenceguard.ErrPolicy) || srv.sentMethod("thread/start") || srv.sentMethod("turn/start") {
		t.Fatal("unsupported path", err)
	}
}
func TestInferenceGuardQuotaEventStopsOtherConversationAndCompaction(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	provider, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	c := provider.(*conversation)
	if _, err = c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "fake first", ClientMessageID: "first"}); err != nil {
		t.Fatal(err)
	}
	// Normalized event injection exercises the exact adapter hook; no native CLI.
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventError, ProviderConversationID: "thread-1", ProviderTurnID: "turn-1", Err: errCodexUsageLimitExceeded, UsageLimitResetsAt: time.Now().Add(time.Hour)})
	before := 0
	srv.mu.Lock()
	for _, f := range srv.seen {
		if f.Method == "turn/start" {
			before++
		}
	}
	srv.mu.Unlock()
	if _, err = c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "blocked", ClientMessageID: "second"}); !errors.Is(err, inferenceguard.ErrQuota) {
		t.Fatal(err)
	}
	if _, err = c.Compact(context.Background()); !errors.Is(err, inferenceguard.ErrQuota) {
		t.Fatal(err)
	}
	after := 0
	srv.mu.Lock()
	for _, f := range srv.seen {
		if f.Method == "turn/start" {
			after++
		}
	}
	srv.mu.Unlock()
	if before != after || srv.sentMethod("thread/compact/start") {
		t.Fatal("request after quota")
	}
	// A new controller obtains the same durable authority.
	other, err := inferenceguard.ForDataDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Check(context.Background()); !errors.Is(err, inferenceguard.ErrQuota) {
		t.Fatal("lockout not global")
	}
}
func TestInferenceGuardDuplicateCompletionCannotReleaseNewLease(t *testing.T) {
	d, _, cfg := guardedDriver(t)
	provider, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	c := provider.(*conversation)
	if _, err = c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "one", ClientMessageID: "first"}); err != nil {
		t.Fatal(err)
	}
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "turn-1"})
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "turn-1", TurnState: domain.TurnStateCompleted})
	c.mu.Lock()
	c.guardRequest = "new-lease"
	c.guardTurn = "turn-2"
	c.mu.Unlock()
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "turn-1", TurnState: domain.TurnStateCompleted})
	c.mu.Lock()
	current := c.guardRequest
	c.mu.Unlock()
	if current != "new-lease" {
		t.Fatal("duplicate old completion settled the new lease")
	}
}

func TestInferenceGuardUnknownAndWrongNativeAccountsFailClosed(t *testing.T) {
	for _, account := range []string{`{"account":null,"requiresOpenaiAuth":true}`, `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`, `{"account":{"type":"chatgpt","email":"wrong@example.invalid"},"requiresOpenaiAuth":true}`} {
		t.Run(account, func(t *testing.T) {
			d, srv, cfg := guardedDriver(t)
			srv.reply("account/read", account)
			_, err := d.Start(context.Background(), cfg)
			if !errors.Is(err, inferenceguard.ErrPolicy) || srv.sentMethod("thread/start") || srv.sentMethod("turn/start") {
				t.Fatal("native identity bypass", err)
			}
		})
	}
}

func TestInferenceGuardQuotaRequestsNativeInterruptWithoutReleasingLease(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	srv.reply("turn/interrupt", "{}")
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "synthetic work", ClientMessageID: "active"}); err != nil {
		t.Fatal(err)
	}
	c := conv.(*conversation)
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventError, Err: errors.New("usage limit exceeded")})
	f := srv.awaitFrame(func(f frame) bool { return f.Method == "turn/interrupt" })
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if err := json.Unmarshal(f.Params, &params); err != nil || params.ThreadID != "thread-1" || params.TurnID == "" {
		t.Fatalf("%s %v", f.Params, err)
	}
	s, err := c.guard.Snapshot(context.Background())
	if err != nil || s.Active != 1 || s.Quota != "EXHAUSTED" {
		t.Fatalf("interrupt acknowledgement prematurely released lease: %+v %v", s, err)
	}
}

func TestInferenceGuardSteerWithoutEffortEnvelopeFailsClosed(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	c := conv.(*conversation)
	_, err = c.Steer(context.Background(), "active-native", ports.ChatUserMessage{Text: "follow-up", Settings: ports.ChatTurnSettings{Model: "gpt-test", Effort: "medium"}})
	if !errors.Is(err, inferenceguard.ErrPolicy) || srv.sentMethod("turn/steer") {
		t.Fatalf("unverifiable steer passed: %v", err)
	}
}
func TestInferenceGuardChildCompletionCannotReleaseRootLease(t *testing.T) {
	d, _, cfg := guardedDriver(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	c := conv.(*conversation)
	c.mu.Lock()
	c.guardRequest = "root-attempt"
	c.guardTurn = "shared-turn-id"
	c.mu.Unlock()
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderConversationID: "different-native-thread", ProviderTurnID: "shared-turn-id", TurnState: domain.TurnStateCompleted})
	c.mu.Lock()
	current := c.guardRequest
	c.mu.Unlock()
	if current != "root-attempt" {
		t.Fatal("child completion changed root guard lease")
	}
}

func TestInferenceGuardStaleRootStartCannotBindPendingRequest(t *testing.T) {
	d, _, cfg := guardedDriver(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	c := conv.(*conversation)
	sel, err := c.guard.Resolve(c.guardRole, c.guardSession, "gpt-test", "medium")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.guard.Dispatch(context.Background(), inferenceguard.Request{ID: "new-pending-request", Session: c.guardSession, Project: c.guardProject, Operation: "turn/start", Selection: sel}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.guardRequest = "new-pending-request"
	c.guardTurn = ""
	c.mu.Unlock()
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderConversationID: "thread-1", ProviderTurnID: "stale-root"})
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderConversationID: "thread-1", ProviderTurnID: "stale-root", TurnState: domain.TurnStateCompleted})
	c.mu.Lock()
	current, native := c.guardRequest, c.guardTurn
	c.mu.Unlock()
	s, err := c.guard.Snapshot(context.Background())
	if err != nil || current != "new-pending-request" || native != "" || s.Active != 1 {
		t.Fatalf("stale replay released new work: request=%s native=%s %+v %v", current, native, s, err)
	}
}
func TestInferenceGuardInternalRPCErrorKeepsUncertainLease(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	srv.replyError("turn/start", -32603, "synthetic acknowledgement failed after acceptance")
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	_, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "fake accepted request", ClientMessageID: "unknown-ack"})
	if err == nil {
		t.Fatal("expected synthetic RPC error")
	}
	c := conv.(*conversation)
	s, err := c.guard.Snapshot(context.Background())
	if err != nil || s.Uncertain != 1 || s.Completed != 0 || s.BudgetUsed != 1 {
		t.Fatalf("unknown outcome freed lease: %+v %v", s, err)
	}
}

func TestInferenceGuardSecondRequestCannotOverwriteActiveStopper(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "first", ClientMessageID: "first"}); err != nil {
		t.Fatal(err)
	}
	c := conv.(*conversation)
	c.mu.Lock()
	firstID, firstTurn := c.guardRequest, c.guardTurn
	c.mu.Unlock()
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "second", ClientMessageID: "second"}); !errors.Is(err, inferenceguard.ErrCapacity) {
		t.Fatal(err)
	}
	c.mu.Lock()
	currentID, currentTurn := c.guardRequest, c.guardTurn
	c.mu.Unlock()
	if currentID != firstID || currentTurn != firstTurn || currentTurn == "" {
		t.Fatalf("lost active stopper: %s %s", currentID, currentTurn)
	}
	srv.mu.Lock()
	writes := 0
	for _, f := range srv.seen {
		if f.Method == "turn/start" {
			writes++
		}
	}
	srv.mu.Unlock()
	if writes != 1 {
		t.Fatalf("wire writes=%d", writes)
	}
}
func TestInferenceGuardCompletionBeforeAcknowledgementSettlesOnlyExactTurn(t *testing.T) {
	d, srv, cfg := guardedDriver(t)
	srv.mu.Lock()
	delete(srv.responses, "turn/start")
	srv.mu.Unlock()
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	c := conv.(*conversation)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := conv.SendTurn(ctx, ports.ChatUserMessage{Text: "fast synthetic work", ClientMessageID: "fast"})
		done <- e
	}()
	f := srv.awaitFrame(func(f frame) bool { return f.Method == "turn/start" })
	c.observeGuardEvent(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderConversationID: "thread-1", ProviderTurnID: "fast-native-turn", TurnState: domain.TurnStateCompleted})
	s, e := c.guard.Snapshot(ctx)
	if e != nil || s.Active != 1 {
		t.Fatalf("unacknowledged completion freed capacity: %+v %v", s, e)
	}
	srv.push("{\"id\":" + string(*f.ID) + ",\"result\":{\"turn\":{\"id\":\"fast-native-turn\"}}}")
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	s, e = c.guard.Snapshot(ctx)
	if e != nil || s.Completed != 1 || s.Active != 0 {
		t.Fatalf("exact acknowledged completion not reconciled: %+v %v", s, e)
	}
}
