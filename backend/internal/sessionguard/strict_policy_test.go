package sessionguard

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
)

func TestStrictPolicyBlocksExistingTerminalMessages(t *testing.T) {
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv("AO_INFERENCE_POLICY", "")
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode, domain.HarnessOpenCode} {
		t.Run(string(harness), func(t *testing.T) {
			rec := record(domain.ActivityActive, false)
			rec.Harness, rec.Mode = harness, domain.SessionModeTUI
			for _, mutation := range []bool{false, true} {
				msgr := &fakeMessenger{}
				g := New(&fakeStore{rec: rec, ok: true}, msgr, nil)
				var outcome Outcome
				var err error
				if mutation {
					outcome, err = g.DeliverUnderMutation(context.Background(), "s1", "implement")
				} else {
					outcome, err = g.Deliver(context.Background(), "s1", "implement")
				}
				if !errors.Is(err, inferenceguard.ErrPolicy) || outcome == Sent || len(msgr.sent) != 0 {
					t.Fatalf("mutation=%v outcome=%v err=%v writes=%d", mutation, outcome, err, len(msgr.sent))
				}
			}
		})
	}
}

func TestStrictPolicyFakeExceptionCannotEnableRealHarness(t *testing.T) {
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv("AO_FAKE_HARNESS", "1")
	rec := record(domain.ActivityActive, false)
	rec.Mode = domain.SessionModeTUI
	rec.Harness = domain.AgentHarness("fake")
	msgr := &fakeMessenger{}
	st := &fakeStore{rec: rec, ok: true}
	g := New(st, msgr, nil)
	if outcome, err := g.Deliver(context.Background(), "s1", "synthetic"); err != nil || outcome != Sent {
		t.Fatalf("fake: %v %v", outcome, err)
	}
	st.rec.Harness = domain.HarnessCodex
	if outcome, err := g.Deliver(context.Background(), "s1", "real"); !errors.Is(err, inferenceguard.ErrPolicy) || outcome == Sent {
		t.Fatalf("real: %v %v", outcome, err)
	}
	if len(msgr.sent) != 1 {
		t.Fatalf("writes=%d", len(msgr.sent))
	}
}
