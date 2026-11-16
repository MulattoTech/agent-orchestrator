package sessionmanager

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func strictLaunchPolicy(t *testing.T) {
	t.Helper()
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "1")
	t.Setenv("AO_INFERENCE_POLICY", "")
}

func TestStrictPolicySpawnRefusesBeforeWorkspaceOrRuntime(t *testing.T) {
	strictLaunchPolicy(t)
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode, domain.HarnessOpenCode} {
		for _, mode := range []domain.SessionMode{domain.SessionModeTUI, domain.SessionModeChat} {
			t.Run(string(harness)+"/"+string(mode), func(t *testing.T) {
				m, st, rt, ws := newManager()
				_, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: harness, RequestedMode: mode, Prompt: "implement"})
				if !errors.Is(err, inferenceguard.ErrPolicy) {
					t.Fatalf("err=%v", err)
				}
				if len(st.sessions) != 0 || rt.created != 0 || rt.destroyed != 0 || len(ws.calls) != 0 {
					t.Fatalf("effects: sessions=%d create=%d destroy=%d workspace=%v", len(st.sessions), rt.created, rt.destroyed, ws.calls)
				}
			})
		}
	}
}

func TestStrictPolicyRestoreResumePreserveExistingTerminal(t *testing.T) {
	strictLaunchPolicy(t)
	for _, restoring := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "restore"}[restoring], func(t *testing.T) {
			rt := &fakeRuntime{}
			m, st, ws := newExitedResumeManager(t, rt, fakeAgent{})
			before := st.sessions["mer-1"]
			before.Harness = domain.HarnessClaudeCode
			before.Mode = domain.SessionModeTUI
			before.IsTerminated = restoring
			st.sessions["mer-1"] = before
			var err error
			if restoring {
				_, err = m.RestoreWithMode(context.Background(), "mer-1")
			} else {
				_, err = m.ResumeAgentWithMode(context.Background(), "mer-1")
			}
			if !errors.Is(err, inferenceguard.ErrPolicy) {
				t.Fatalf("err=%v", err)
			}
			if !reflect.DeepEqual(st.sessions["mer-1"], before) || rt.created != 0 || rt.destroyed != 0 || len(ws.calls) != 0 {
				t.Fatalf("refusal mutated terminal or workspace")
			}
		})
	}
}

func TestStrictPolicyRejectsTargetBeforeStoppingSource(t *testing.T) {
	strictLaunchPolicy(t)
	m, st, rt, ws := newManager()
	rec := mkLive("mer-1")
	rec.Mode, rec.Harness = domain.SessionModeTUI, domain.HarnessClaudeCode
	rec.Activity.State = domain.ActivityIdle
	st.sessions[rec.ID] = rec
	if err := m.SwitchTUIProvider(context.Background(), rec.ID, domain.ProviderSwitchRequest{TargetHarness: domain.HarnessOpenCode}); !errors.Is(err, inferenceguard.ErrPolicy) {
		t.Fatalf("switch err=%v", err)
	}
	if err := m.preflightInterfaceTarget(context.Background(), rec, domain.SessionInterfaceTransition{TargetMode: domain.SessionModeTUI}); !errors.Is(err, inferenceguard.ErrPolicy) {
		t.Fatalf("interface err=%v", err)
	}
	if _, err := m.TerminalForkSource(context.Background(), rec, ""); !errors.Is(err, inferenceguard.ErrPolicy) {
		t.Fatalf("fork err=%v", err)
	}
	if !reflect.DeepEqual(st.sessions[rec.ID], rec) || rt.created != 0 || rt.destroyed != 0 || len(rt.interrupts) != 0 || len(ws.calls) != 0 {
		t.Fatal("target refusal changed source")
	}
}

func TestStrictPolicyDisablesRawTerminalInputLease(t *testing.T) {
	strictLaunchPolicy(t)
	m, st, _, _ := newManager()
	rec := mkLive("mer-1")
	rec.Harness, rec.Mode = domain.HarnessClaudeCode, domain.SessionModeTUI
	st.sessions[rec.ID] = rec
	for _, id := range []domain.SessionID{rec.ID, "unknown-reviewer"} {
		if release, ok := m.AcquireSessionInput(id); ok {
			release()
			t.Fatalf("raw input admitted for %q", id)
		}
	}
}
