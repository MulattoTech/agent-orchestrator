package review

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestStrictPolicyReviewerTerminalRefusesBeforeEffects(t *testing.T) {
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv("AO_INFERENCE_POLICY", "")
	for _, op := range []string{"preflight", "spawn", "restore", "notify"} {
		t.Run(op, func(t *testing.T) {
			rt := &fakeRuntime{}
			reviewer := &fakeReviewer{}
			dir := t.TempDir()
			launcher := NewLauncher(fakeReviewerResolver{reviewer: reviewer, ok: true}, rt, dir)
			spec := launchSpec()
			spec.InterfaceMode = domain.ReviewerInterfaceTUI
			var err error
			switch op {
			case "preflight":
				err = launcher.Preflight(context.Background(), spec.Harness, spec.WorkspacePath, domain.ReviewerInterfaceTUI)
			case "spawn":
				_, err = launcher.Spawn(context.Background(), spec)
			case "restore":
				_, err = launcher.RestoreTerminal(context.Background(), spec)
			case "notify":
				err = launcher.Notify(context.Background(), "existing-reviewer", spec)
			}
			if !errors.Is(err, inferenceguard.ErrPolicy) {
				t.Fatalf("err=%v", err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if rt.created || rt.destroyed != "" || len(rt.sentMsgs) != 0 || reviewer.gotInv.RunID != "" || len(entries) != 0 {
				t.Fatalf("refusal had side effects: runtime=%+v files=%d", rt, len(entries))
			}
		})
	}
}

type strictMessageCanceller struct{ fakeReviewer }

func (strictMessageCanceller) ReviewCancel(context.Context) (ports.ReviewCancelSpec, error) {
	return ports.ReviewCancelSpec{Mode: ports.ReviewCancelMessage, Message: "stop"}, nil
}

func TestStrictPolicyCancelUsesInterruptWithoutSendingPrompt(t *testing.T) {
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	rt := &fakeRuntime{}
	launcher := newTestLauncher(t, &strictMessageCanceller{}, rt)
	if err := launcher.Cancel(context.Background(), "existing-reviewer", domain.ReviewerClaudeCode); err != nil {
		t.Fatal(err)
	}
	if rt.interrupt != "existing-reviewer" || len(rt.sentMsgs) != 0 {
		t.Fatalf("cancel: interrupt=%q writes=%v", rt.interrupt, rt.sentMsgs)
	}
}
