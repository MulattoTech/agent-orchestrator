// Package nativereceipts reads provider-owned JSONL files. It never starts a
// provider, reads credentials, sends network requests, or stores transcript text.
package nativereceipts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

const MaxLineBytes = 4 << 20

var ErrUnknownContext = errors.New("usage model or effort context unavailable")
var ErrWrongThread = errors.New("native thread does not match durable AO ownership")

type Binding struct {
	Session string
	Thread  string
}
type Sink interface {
	IngestBoundReceipt(context.Context, inferenceguard.UsageReceipt) error
}
type turnContext struct {
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}
type Parser struct {
	binding  Binding
	verified bool
	contexts map[string]turnContext
	pending  map[string]inferenceguard.UsageReceipt
}

func NewParser(binding Binding) *Parser {
	return &Parser{binding: binding, contexts: map[string]turnContext{}, pending: map[string]inferenceguard.UsageReceipt{}}
}

// Line ignores message/tool content and cumulative usage fields. The source must
// first establish its exact native thread identity; unknown usage stays unknown.
func (p *Parser) Line(ctx context.Context, data []byte, sink Sink) (bool, error) {
	if len(data) > MaxLineBytes {
		return false, errors.New("native_record_too_large")
	}
	var e struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return false, errors.New("native_record_malformed")
	}
	switch e.Type {
	case "session_meta":
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e.Payload, &m) != nil || m.ID != p.binding.Thread {
			return false, ErrWrongThread
		}
		p.verified = true
	case "turn_context":
		if !p.verified {
			return false, ErrWrongThread
		}
		var c turnContext
		if err := json.Unmarshal(e.Payload, &c); err != nil {
			return false, errors.New("native_context_malformed")
		}
		if c.TurnID == "" || c.Model == "" || c.Effort == "" {
			return false, ErrUnknownContext
		}
		c.Effort = strings.ToLower(c.Effort)
		// Bound memory even for multi-month logs; missing older context is reported,
		// never silently inferred from a later unrelated turn.
		if len(p.contexts) >= 4096 {
			p.contexts = map[string]turnContext{}
		}
		p.contexts[c.TurnID] = c
	case "token_usage_record":
		if !p.verified {
			return false, ErrWrongThread
		}
		var u struct {
			Thread   string `json:"thread_id"`
			Turn     string `json:"turn_id"`
			Root     string `json:"root_turn_id"`
			Response string `json:"response_id"`
			Usage    *struct {
				Input     *int64 `json:"input_tokens"`
				Cached    *int64 `json:"cached_input_tokens"`
				Output    *int64 `json:"output_tokens"`
				Reasoning *int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(e.Payload, &u) != nil {
			return false, errors.New("native_usage_malformed")
		}
		if u.Thread != p.binding.Thread {
			return false, ErrWrongThread
		}
		root := u.Root
		if root == "" {
			root = u.Turn
		}
		if root == "" || u.Response == "" || e.Timestamp.IsZero() || u.Usage == nil || u.Usage.Input == nil || u.Usage.Cached == nil || u.Usage.Output == nil || u.Usage.Reasoning == nil {
			return false, errors.New("native_usage_identity_or_counters_missing")
		}
		c, ok := p.contexts[u.Turn]
		if !ok {
			c, ok = p.contexts[root]
		}
		if !ok {
			return false, ErrUnknownContext
		}
		r := inferenceguard.UsageReceipt{SourceKey: "codex:response:" + u.Response, NativeThreadID: p.binding.Thread, ProviderTurnID: root, Session: p.binding.Session,
			Model: c.Model, Effort: c.Effort, ObservedAt: e.Timestamp.UTC(), InputTokens: *u.Usage.Input,
			CachedInputTokens: *u.Usage.Cached, OutputTokens: *u.Usage.Output, ReasoningOutputTokens: *u.Usage.Reasoning, Confidence: "provider_response"}
		if r.InputTokens < 0 || r.CachedInputTokens < 0 || r.OutputTokens < 0 || r.ReasoningOutputTokens < 0 || r.CachedInputTokens > r.InputTokens || r.ReasoningOutputTokens > r.OutputTokens {
			return false, errors.New("native_usage_counters_invalid")
		}
		if err := sink.IngestBoundReceipt(ctx, r); err != nil {
			if errors.Is(err, inferenceguard.ErrUnboundReceipt) && time.Since(r.ObservedAt) < 5*time.Minute && len(p.pending) < 128 {
				p.pending[r.SourceKey] = r
			}
			return false, fmt.Errorf("receipt ingest: %w", err)
		}
		return true, nil
	}
	return false, nil
}

// RetryPending closes the small receipt-before-RPC-binding race without attributing historical accounts.
func (p *Parser) RetryPending(ctx context.Context, sink Sink) (int, error) {
	accepted := 0
	for id, r := range p.pending {
		err := sink.IngestBoundReceipt(ctx, r)
		if err == nil {
			delete(p.pending, id)
			accepted++
			continue
		}
		if !errors.Is(err, inferenceguard.ErrUnboundReceipt) {
			return accepted, err
		}
		if time.Since(r.ObservedAt) > 5*time.Minute {
			delete(p.pending, id)
		}
	}
	return accepted, nil
}
