package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureChatDriver struct {
	effects     bootFixtureEffects
	root, phase string
}

func (d *fixtureChatDriver) Harness() domain.AgentHarness { return domain.HarnessCodex }
func (d *fixtureChatDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	d.effects.Allow("chat.synthetic-probe")
	return fixtureChatCapabilities(), nil
}
func fixtureChatCapabilities() ports.ChatCapabilities {
	return ports.ChatCapabilities{
		ports.ChatCapabilityStreaming: true,
		ports.ChatCapabilityApprovals: true,
		ports.ChatCapabilityResume:    true,
		ports.ChatCapabilityInterrupt: true,
	}
}
func (d *fixtureChatDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return nil, d.effects.Deny("chat.unconfigured-new-conversation")
}
func (d *fixtureChatDriver) Resume(ctx context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.open(ctx, cfg)
}
func (d *fixtureChatDriver) Reconnect(ctx context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.open(ctx, cfg)
}
func (d *fixtureChatDriver) open(ctx context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	if !strings.HasPrefix(string(cfg.SessionID), "fixture-") || cfg.ProviderConversationID != "synthetic-thread-"+string(cfg.SessionID) || !filepath.IsAbs(cfg.WorkspacePath) {
		return nil, d.effects.Deny("chat.unqualified-owner")
	}
	relative, err := filepath.Rel(filepath.Join(d.root, "workspaces"), cfg.WorkspacePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, d.effects.Deny("chat.workspace-outside-root")
	}
	if cfg.Model != fixtureAgentModel || cfg.Effort != fixtureAgentEffort {
		return nil, d.effects.Deny("chat.model-policy-mismatch")
	}
	if d.phase == "recovery" && cfg.SessionID == "fixture-project-3" {
		d.effects.Allow("chat.synthetic-uncertain-reconnect")
		return nil, fmt.Errorf("%w: synthetic dispatch outcome remains unknown", ports.ErrRuntimeProbeInconclusive)
	}
	gate, err := inferenceguard.ForDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, d.effects.Deny("chat.missing-guard")
	}
	policy, err := inferenceguard.ReadPolicy()
	if err != nil {
		return nil, err
	}
	// This proves only the configured synthetic fixture identity, never a real account.
	if err = gate.VerifyAccount(ctx, policy.NativeAccountSHA256); err != nil {
		return nil, err
	}
	if err = gate.RegisterNativeThread(ctx, string(cfg.SessionID), cfg.ProviderConversationID); err != nil {
		return nil, err
	}
	d.effects.Allow("chat.synthetic-reconnect")
	return &fixtureChatConversation{driver: d, cfg: cfg, gate: gate, events: make(chan ports.ChatEvent, 8)}, nil
}

type fixtureChatConversation struct {
	driver           *fixtureChatDriver
	cfg              ports.ChatResumeConfig
	gate             *inferenceguard.Gate
	events           chan ports.ChatEvent
	once             sync.Once
	mu               sync.Mutex
	closed           bool
	pendingApprovals map[string]string
}

func (c *fixtureChatConversation) ProviderConversationID() string {
	return c.cfg.ProviderConversationID
}
func (c *fixtureChatConversation) Capabilities() ports.ChatCapabilities {
	return fixtureChatCapabilities()
}
func (c *fixtureChatConversation) ReconnectedLive() bool          { return true }
func (c *fixtureChatConversation) PreservesProviderOnClose() bool { return true }
func (c *fixtureChatConversation) Events() <-chan ports.ChatEvent { return c.events }
func (c *fixtureChatConversation) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		clear(c.pendingApprovals)
		close(c.events)
		c.mu.Unlock()
		c.driver.effects.Allow("chat.controller-detach")
	})
	return nil
}
func (c *fixtureChatConversation) emit(ctx context.Context, event ports.ChatEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.emitLocked(ctx, event)
}
func (c *fixtureChatConversation) emitLocked(ctx context.Context, event ports.ChatEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed {
		return errors.New("synthetic conversation is closed")
	}
	select {
	case c.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		// A fixed bounded fake must fail rather than block Close indefinitely.
		return c.driver.effects.Deny("chat.synthetic-stream-backpressure")
	}
}
func (c *fixtureChatConversation) Interrupt(context.Context, string) error {
	return c.driver.effects.Deny("chat.unconfigured-interrupt")
}

// requestApproval represents a synthetic pending decision only. It never starts
// a command or changes a provider turn. The fixed lifecycle workload does not
// request approvals; contract tests explicitly exercise the supported channel.
func (c *fixtureChatConversation) requestApproval(ctx context.Context, requestID, turnID string) error {
	if !strings.HasPrefix(requestID, "synthetic-approval-") || !strings.HasPrefix(turnID, "synthetic-turn-") {
		return c.driver.effects.Deny("chat.unqualified-approval")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingApprovals == nil {
		c.pendingApprovals = map[string]string{}
	}
	if _, exists := c.pendingApprovals[requestID]; exists {
		return c.driver.effects.Deny("chat.duplicate-approval")
	}
	if err := c.emitLocked(ctx, ports.ChatEvent{
		Kind: ports.ChatEventApprovalRequested, ProviderConversationID: c.cfg.ProviderConversationID,
		ProviderTurnID: turnID, RequestID: requestID,
		Summary: "Synthetic decision; no command or model will execute.",
		Decisions: []ports.ChatDecisionOption{
			{ID: "fixture-allow-once", Kind: ports.ChatDecisionAllowOnce},
			{ID: "fixture-reject-once", Kind: ports.ChatDecisionRejectOnce},
		},
	}); err != nil {
		return err
	}
	c.pendingApprovals[requestID] = turnID
	c.driver.effects.Allow("chat.synthetic-approval-requested")
	return nil
}

func (c *fixtureChatConversation) ResolveRequest(ctx context.Context, requestID string, decision ports.ChatDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	turnID, pending := c.pendingApprovals[requestID]
	if c.closed || !pending {
		return errors.Join(ports.ErrChatRequestNotPending, c.driver.effects.Deny("chat.unconfigured-approval"))
	}
	if (decision.ID != "fixture-allow-once" && decision.ID != "fixture-reject-once") || len(decision.Raw) != 0 {
		return errors.Join(ports.ErrChatDecisionNotOffered, c.driver.effects.Deny("chat.unoffered-approval-decision"))
	}
	if err := c.emitLocked(ctx, ports.ChatEvent{
		Kind: ports.ChatEventApprovalResolved, ProviderConversationID: c.cfg.ProviderConversationID,
		ProviderTurnID: turnID, RequestID: requestID,
	}); err != nil {
		return err
	}
	delete(c.pendingApprovals, requestID)
	c.driver.effects.Allow("chat.synthetic-approval-resolved")
	return nil
}
func (c *fixtureChatConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if err := ctx.Err(); err != nil {
		return ports.ChatTurnRef{}, err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ports.ChatTurnRef{}, c.driver.effects.Deny("chat.detached-provider-write")
	}
	p, err := inferenceguard.ReadPolicy()
	if err != nil {
		return ports.ChatTurnRef{}, err
	}
	model, effort := msg.Settings.Model, msg.Settings.Effort
	if model == "" {
		model = c.cfg.Model
	}
	if effort == "" {
		effort = c.cfg.Effort
	}
	selected, err := p.Resolve(c.cfg.PolicyRole, string(c.cfg.SessionID), model, effort, time.Now())
	if err != nil {
		return ports.ChatTurnRef{}, err
	}
	key := sha256.Sum256([]byte(string(c.cfg.SessionID) + "\x00" + msg.ClientMessageID))
	requestID := "synthetic-" + hex.EncodeToString(key[:])
	nativeTurn := "synthetic-turn-" + msg.ClientMessageID
	err = c.gate.Dispatch(ctx, inferenceguard.Request{ID: requestID, Session: string(c.cfg.SessionID), Project: string(c.cfg.ProjectID), NativeThreadID: c.cfg.ProviderConversationID, Operation: "turn/start", Selection: selected}, func() error {
		// Close and the synthetic write have one ownership fence. A shutdown
		// racing admission may not write through a detached controller.
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed {
			return c.driver.effects.Deny("chat.detached-provider-write")
		}
		if (msg.ClientMessageID != "fixture-complete" || c.cfg.SessionID != "fixture-project-2") && (msg.ClientMessageID != "fixture-uncertain" || c.cfg.SessionID != "fixture-project-3") {
			return c.driver.effects.Deny("chat.unconfigured-provider-write")
		}
		c.driver.effects.Allow("chat.synthetic-provider-write")
		// Immutable fake receipt: opened exclusively, so any duplicate invocation
		// is a failure instead of an apparently successful second delivery.
		path := filepath.Join(c.driver.root, "synthetic-writes", msg.ClientMessageID+".json")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]string{"session": string(c.cfg.SessionID), "request": requestID, "turn": nativeTurn, "model": selected.Model, "effort": selected.Effort, "provenance": "synthetic"})
		if err == nil {
			_, err = f.Write(body)
		}
		if err == nil {
			err = f.Sync()
		}
		return errors.Join(err, f.Close())
	})
	if err != nil {
		return ports.ChatTurnRef{}, err
	}
	if err = c.gate.Bind(ctx, requestID, nativeTurn); err != nil {
		return ports.ChatTurnRef{}, err
	}
	if err = c.emit(ctx, ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: nativeTurn, ProviderConversationID: c.cfg.ProviderConversationID}); err != nil {
		return ports.ChatTurnRef{}, err
	}
	if msg.ClientMessageID == "fixture-complete" {
		if err = c.gate.SettleRequest(ctx, requestID, ""); err != nil {
			return ports.ChatTurnRef{}, err
		}
		if err = c.emit(ctx, ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: nativeTurn, ProviderConversationID: c.cfg.ProviderConversationID, ProviderItemID: "synthetic-answer", Text: "Synthetic completion; no model executed."}); err != nil {
			return ports.ChatTurnRef{}, err
		}
		if err = c.emit(ctx, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: nativeTurn, ProviderConversationID: c.cfg.ProviderConversationID, TurnState: domain.TurnStateCompleted}); err != nil {
			return ports.ChatTurnRef{}, err
		}
	}
	return ports.ChatTurnRef{ProviderTurnID: nativeTurn}, nil
}
func TestFixtureChatRejectsMissingOrExcessiveModelPolicy(t *testing.T) {
	for _, choice := range []struct{ name, model, effort string }{
		{"missing effort", fixtureAgentModel, ""},
		{"high effort", fixtureAgentModel, "high"},
		{"wrong model", "unexpected-model", fixtureAgentEffort},
	} {
		t.Run(choice.name, func(t *testing.T) {
			root := t.TempDir()
			effects := &fixtureEffectRecorder{}
			driver := &fixtureChatDriver{root: root, phase: "crash", effects: effects}
			_, err := driver.Resume(context.Background(), ports.ChatResumeConfig{
				SessionID: "fixture-project-2", ProviderConversationID: "synthetic-thread-fixture-project-2",
				WorkspacePath: filepath.Join(root, "workspaces", "fixture-project-2"),
				Model:         choice.model, Effort: choice.effort,
			})
			if err == nil || len(effects.entries) != 1 || effects.entries[0].Kind != "forbidden" || effects.entries[0].Name != "chat.model-policy-mismatch" {
				t.Fatalf("policy error=%v effects=%+v", err, effects.entries)
			}
		})
	}
}
