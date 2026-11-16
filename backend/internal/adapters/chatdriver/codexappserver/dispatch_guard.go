package codexappserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver/codexproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func prepareDispatchGuard(dataDir, session, role, model, effort string, readOnly bool) (*inferenceguard.Gate, inferenceguard.Selection, error) {
	if !inferenceguard.Enabled() {
		return nil, inferenceguard.Selection{}, nil
	}
	if role == "" {
		role = "implementation"
		if readOnly {
			role = "reviewer"
		}
	}
	g, err := inferenceguard.ForDataDir(dataDir)
	if err != nil {
		return nil, inferenceguard.Selection{}, err
	}
	if err := g.Check(context.Background()); err != nil {
		return nil, inferenceguard.Selection{}, err
	}
	s, err := g.Resolve(role, session, model, effort)
	return g, s, err
}
func (c *conversation) attachDispatchGuard(g *inferenceguard.Gate, s inferenceguard.Selection, session string, project domain.ProjectID) {
	c.guard = g
	c.guardSession = session
	c.guardProject = string(project)
	c.guardRole = s.Role
	if g != nil {
		c.guardUnsubscribe = g.RegisterStopper(c.interruptGuardedTurn)
	}
}
func (c *conversation) resolveGuardSelection(ctx context.Context, model, effort string) (inferenceguard.Selection, error) {
	if c.guard == nil {
		return inferenceguard.Selection{}, nil
	}
	if err := c.guard.Check(ctx); err != nil {
		return inferenceguard.Selection{}, err
	}
	selection, err := c.guard.Resolve(c.guardRole, c.guardSession, model, effort)
	if err != nil {
		return selection, err
	}
	if err = c.verifyGuardCatalog(ctx, selection); err != nil {
		return selection, err
	}
	// A supported non-inference quota read may latch a stop before this turn.
	// Missing quota data remains UNKNOWN and uses the finite owner turn budget.
	quotaCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, _ = c.ReadRateLimits(quotaCtx)
	cancel()
	if err := c.guard.Check(ctx); err != nil {
		return selection, err
	}
	return selection, nil
}
func (c *conversation) verifyGuardCatalog(ctx context.Context, s inferenceguard.Selection) error {
	if err := c.verifyGuardAccount(ctx); err != nil {
		return err
	}
	// model/list is a native non-inference read. Never guess support from a UI name.
	models, err := listModels(ctx, c.conn)
	if err != nil {
		return &inferenceguard.Refusal{Code: inferenceguard.ErrPolicy, Reason: "cannot verify provider model capability"}
	}
	for _, m := range models {
		if m.ID != s.Model {
			continue
		}
		for _, effort := range m.Efforts {
			if effort == s.Effort {
				return nil
			}
		}
		return &inferenceguard.Refusal{Code: inferenceguard.ErrPolicy, Reason: "provider does not advertise requested effort for model"}
	}
	return &inferenceguard.Refusal{Code: inferenceguard.ErrPolicy, Reason: "provider does not advertise requested model"}
}
func (c *conversation) guardedRequest(ctx context.Context, id, method string, params, out any, s inferenceguard.Selection) error {
	if c.guard == nil {
		return c.conn.request(ctx, method, params, out)
	}
	err := c.conn.requestWithWrite(ctx, method, params, out, func(write func() error) error {
		return c.guard.Dispatch(ctx, inferenceguard.Request{NativeThreadID: c.threadID, ID: id, Session: c.guardSession, Project: c.guardProject, Operation: method, Selection: s}, func() error {
			c.mu.Lock()
			c.guardRequest = id
			c.guardTurn = ""
			c.mu.Unlock()
			return write()
		})
	})
	if err != nil {
		if isGuardQuotaError(err) {
			if tripErr := c.guard.Trip(context.WithoutCancel(ctx), time.Time{}, "provider_usage_limit_without_authoritative_reset"); tripErr != nil {
				return errors.Join(err, tripErr)
			}
		}
		var rpcErr *rpcError
		// Only protocol validation/method refusals establish a rejected request.
		// Internal/server errors can follow acceptance and are not proof of no work.
		if errors.As(err, &rpcErr) && (rpcErr.Code == -32600 || rpcErr.Code == -32601 || rpcErr.Code == -32602) {
			_ = c.guard.SettleRequest(context.WithoutCancel(ctx), id, "provider_validation_refusal")
		} else if !inferenceguard.PreDispatchRefusal(err) {
			_ = c.guard.MarkUncertain(context.WithoutCancel(ctx), id, "provider_acknowledgement_unknown")
		}
		c.mu.Lock()
		if c.guardRequest == id {
			c.guardRequest = ""
		}
		c.mu.Unlock()
	}
	return err
}
func isGuardQuotaError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errCodexUsageLimitExceeded) || strings.Contains(strings.ToLower(err.Error()), "usage limit") || strings.Contains(strings.ToLower(err.Error()), "usagelimitexceeded")
}
func (c *conversation) observeGuardEvent(ev ports.ChatEvent) {
	if c.guard == nil {
		return
	}
	if ev.RateLimits != nil && (ev.RateLimits.PrimaryUsedPercent >= 100 || ev.RateLimits.SecondaryUsedPercent >= 100) {
		reset := time.Time{}
		if cap := ev.RateLimits.CodexCapacity; cap != nil && cap.AutoResumeResetsAt != nil {
			reset = *cap.AutoResumeResetsAt
		}
		if err := c.guard.Trip(context.Background(), reset, "native_account_rate_limit"); err != nil {
			c.log.Error("quota authority persistence failed", "error", err)
		}
	}
	// Observe quota before event publication, so controller auto-resume cannot race
	// the durable lockout. A reset hint is never permission to clear the breaker.
	if isGuardQuotaError(ev.Err) {
		if err := c.guard.Trip(context.Background(), ev.UsageLimitResetsAt, "native_usage_limit"); err != nil {
			c.log.Error("quota authority persistence failed; dispatch must remain closed", "error", err)
		}
	}
	if ev.ProviderConversationID != "" && ev.ProviderConversationID != c.threadID {
		return
	}
	if ev.ProviderTurnID == "" {
		return
	}
	if ev.Kind == ports.ChatEventTurnStarted {
		// A replayed root start is not correlated to our pending request. Only the
		// turn/start RPC acknowledgement may bind that request to a native turn.
		return
	}
	if ev.Kind != ports.ChatEventTurnCompleted {
		return
	}
	errorClass := ""
	if ev.Err != nil || ev.TurnState == domain.TurnStateFailed {
		errorClass = "provider_failure"
	}
	c.mu.Lock()
	if c.guardCompleted == nil {
		c.guardCompleted = map[string]string{}
	}
	// Only the current start acknowledgement can need this race buffer.
	// Bound historical/replayed completion IDs during long-running sessions.
	if len(c.guardCompleted) >= 64 {
		clear(c.guardCompleted)
	}
	c.guardCompleted[ev.ProviderTurnID] = errorClass
	id := ""
	if c.guardTurn == ev.ProviderTurnID {
		id = c.guardRequest
		c.guardRequest = ""
		c.guardTurn = ""
	}
	c.mu.Unlock()
	if id != "" {
		if err := c.guard.SettleRequest(context.Background(), id, errorClass); err != nil {
			c.log.Error("guard settlement failed", "error", err)
		}
	} else {
		_ = c.guard.Complete(context.Background(), c.guardSession, ev.ProviderTurnID, errorClass)
	}
}

// InferenceSnapshot is a local database read, never a provider request.
func (c *conversation) InferenceSnapshot(ctx context.Context) (inferenceguard.Snapshot, error) {
	if c.guard == nil {
		return inferenceguard.Snapshot{}, fmt.Errorf("strict dispatch authority not configured")
	}
	return c.guard.Snapshot(ctx)
}

func (c *conversation) verifyGuardAccount(ctx context.Context) error {
	refresh := false
	var response codexproto.GetAccountResponse
	if err := c.conn.request(ctx, codexproto.MethodAccountRead, codexproto.GetAccountParams{RefreshToken: &refresh}, &response); err != nil {
		return &inferenceguard.Refusal{Code: inferenceguard.ErrPolicy, Reason: "native account identity could not be read safely"}
	}
	if response.Account == nil || response.Account.Type != codexproto.AccountTypeChatgpt || response.Account.Email == nil || strings.TrimSpace(*response.Account.Email) == "" {
		return &inferenceguard.Refusal{Code: inferenceguard.ErrPolicy, Reason: "native identity is unknown or not the configured ChatGPT account"}
	}
	return c.guard.VerifyAccount(ctx, inferenceguard.AccountFingerprint("chatgpt", *response.Account.Email))
}

// interruptGuardedTurn never guesses a native turn or kills a provider process.
// The lease remains occupied until the native completion event is observed.
func (c *conversation) interruptGuardedTurn(ctx context.Context) error {
	c.mu.Lock()
	turn := c.guardTurn
	c.mu.Unlock()
	if turn == "" {
		return nil
	}
	err := c.Interrupt(ctx, turn)
	if err != nil && !errors.Is(err, ports.ErrChatNoActiveTurn) {
		c.log.Error("governed native interrupt not acknowledged", "error", err)
		return err
	}
	return nil
}
func (c *conversation) interruptIfGuardStopped() {
	if c.guard == nil {
		return
	}
	stopped, err := c.guard.HardStopped(context.Background())
	if err != nil {
		c.log.Error("read hard-stop authority", "error", err)
	}
	if stopped {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = c.interruptGuardedTurn(ctx)
		}()
	}
}

func (c *conversation) startGuarded(ctx context.Context, thread, model, effort string) error {
	if c.guard != nil {
		if err := c.guard.RegisterNativeThread(ctx, c.guardSession, thread); err != nil {
			return err
		}
	}
	c.start(thread, model, effort)
	return nil
}
