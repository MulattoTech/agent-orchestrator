package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"

	chatdriverregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestFixtureChatSupportsDefaultPermissionAdmission(t *testing.T) {
	// This unit invokes only an explicitly supplied fake, without a provider or
	// account factory. The packaged lifecycle still requires its owner policy.
	t.Setenv(inferenceguard.PolicyEnv, "")
	t.Setenv("AO_REQUIRE_INFERENCE_POLICY", "")
	effects := &fixtureEffectRecorder{}
	driver := &fixtureChatDriver{effects: effects, root: t.TempDir(), phase: "crash"}
	service := chatsvc.New(chatsvc.Options{
		Drivers:             chatdriverregistry.New(driver),
		FileSnapshotCapture: fixtureFileSnapshotCapture(effects),
	})
	if err := service.PreflightChat(context.Background(), domain.HarnessCodex, ports.PermissionModeDefault); err != nil {
		t.Fatalf("fake driver violates the real default-permission admission contract: %v", err)
	}
	if len(effects.entries) != 1 || effects.entries[0].Kind != "allowed" || effects.entries[0].Name != "chat.synthetic-probe" {
		t.Fatalf("unexpected admission effects: %+v", effects.entries)
	}
}

func newFixtureApprovalConversation(effects bootFixtureEffects) *fixtureChatConversation {
	return &fixtureChatConversation{
		driver: &fixtureChatDriver{effects: effects},
		cfg:    ports.ChatResumeConfig{ProviderConversationID: "synthetic-thread-fixture-project-2"},
		events: make(chan ports.ChatEvent, 8),
	}
}

func TestFixtureChatApprovalDecisionsStayCorrelated(t *testing.T) {
	for _, decisionID := range []string{"fixture-allow-once", "fixture-reject-once"} {
		t.Run(decisionID, func(t *testing.T) {
			effects := &fixtureEffectRecorder{}
			conversation := newFixtureApprovalConversation(effects)
			defer conversation.Close()
			const requestID = "synthetic-approval-contract"
			const turnID = "synthetic-turn-contract"
			if err := conversation.requestApproval(context.Background(), requestID, turnID); err != nil {
				t.Fatal(err)
			}
			request := <-conversation.Events()
			if request.Kind != ports.ChatEventApprovalRequested || request.RequestID != requestID || request.ProviderTurnID != turnID || len(request.Decisions) != 2 {
				t.Fatalf("uncorrelated approval request: %+v", request)
			}
			if request.Decisions[0].Kind != ports.ChatDecisionAllowOnce || request.Decisions[1].Kind != ports.ChatDecisionRejectOnce {
				t.Fatalf("approval consent meanings: %+v", request.Decisions)
			}
			if err := conversation.ResolveRequest(context.Background(), requestID, ports.ChatDecision{ID: decisionID}); err != nil {
				t.Fatal(err)
			}
			resolved := <-conversation.Events()
			if resolved.Kind != ports.ChatEventApprovalResolved || resolved.RequestID != requestID || resolved.ProviderTurnID != turnID || resolved.ProviderConversationID != conversation.ProviderConversationID() {
				t.Fatalf("uncorrelated approval resolution: %+v", resolved)
			}
			if len(effects.entries) != 2 || effects.entries[0].Name != "chat.synthetic-approval-requested" || effects.entries[1].Name != "chat.synthetic-approval-resolved" {
				t.Fatalf("approval invoked unexpected effects: %+v", effects.entries)
			}
			if err := conversation.ResolveRequest(context.Background(), requestID, ports.ChatDecision{ID: decisionID}); !errors.Is(err, ports.ErrChatRequestNotPending) {
				t.Fatalf("duplicate resolution error=%v", err)
			}
			if len(conversation.events) != 0 || effects.entries[2].Kind != "forbidden" || effects.entries[2].Name != "chat.unconfigured-approval" {
				t.Fatalf("duplicate resolution emitted an event or escaped denial: %+v", effects.entries)
			}
		})
	}
}

func TestFixtureChatApprovalRejectsUnknownAndUnofferedDecisions(t *testing.T) {
	effects := &fixtureEffectRecorder{}
	conversation := newFixtureApprovalConversation(effects)
	defer conversation.Close()
	if err := conversation.ResolveRequest(context.Background(), "foreign-request", ports.ChatDecision{ID: "fixture-allow-once"}); !errors.Is(err, ports.ErrChatRequestNotPending) {
		t.Fatalf("unknown request error=%v", err)
	}
	const requestID = "synthetic-approval-retained"
	if err := conversation.requestApproval(context.Background(), requestID, "synthetic-turn-contract"); err != nil {
		t.Fatal(err)
	}
	<-conversation.Events()
	for _, decision := range []ports.ChatDecision{
		{ID: "allow-always"},
		{ID: "fixture-allow-once", Raw: []byte(`{"unoffered":true}`)},
	} {
		if err := conversation.ResolveRequest(context.Background(), requestID, decision); !errors.Is(err, ports.ErrChatDecisionNotOffered) {
			t.Fatalf("unoffered decision error=%v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conversation.ResolveRequest(canceled, requestID, ports.ChatDecision{ID: "fixture-reject-once"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution error=%v", err)
	}
	if len(conversation.events) != 0 {
		t.Fatal("invalid decision consumed or resolved the pending approval")
	}
	if err := conversation.ResolveRequest(context.Background(), requestID, ports.ChatDecision{ID: "fixture-reject-once"}); err != nil {
		t.Fatalf("valid decision after refusals: %v", err)
	}
	var forbidden int
	for _, effect := range effects.entries {
		if effect.Kind == "forbidden" {
			forbidden++
		}
	}
	if forbidden != 3 {
		t.Fatalf("invalid requests were not counted exactly: %+v", effects.entries)
	}
}

func TestFixtureChatEventsRefuseBackpressureAndClosedStreams(t *testing.T) {
	effects := &fixtureEffectRecorder{}
	conversation := newFixtureApprovalConversation(effects)
	for i := 0; i < cap(conversation.events); i++ {
		if err := conversation.emit(context.Background(), ports.ChatEvent{Kind: ports.ChatEventControllerState}); err != nil {
			t.Fatal(err)
		}
	}
	if err := conversation.emit(context.Background(), ports.ChatEvent{Kind: ports.ChatEventControllerState}); err == nil {
		t.Fatal("full bounded fake stream accepted another event")
	}
	if len(effects.entries) != 1 || effects.entries[0].Kind != "forbidden" || effects.entries[0].Name != "chat.synthetic-stream-backpressure" {
		t.Fatalf("backpressure did not fail closed: %+v", effects.entries)
	}
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conversation.emit(context.Background(), ports.ChatEvent{Kind: ports.ChatEventControllerState}); err == nil {
		t.Fatal("closed fake stream accepted another event")
	}
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if len(effects.entries) != 2 || effects.entries[1].Name != "chat.controller-detach" {
		t.Fatalf("Close was not idempotent: %+v", effects.entries)
	}
}

// fixtureConversationReady inspects the public, read-only conversation view.
// No send, wake, retry, approval or provider operation is hidden in this check.
func fixtureConversationReady(body []byte, sessionID string) (bool, string, error) {
	var snapshot controllers.ConversationSnapshotResponse
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return false, "", err
	}
	if snapshot.SessionID != sessionID || snapshot.Harness != string(domain.HarnessCodex) || snapshot.Mode != string(domain.SessionModeChat) {
		return false, snapshot.Controller, fmt.Errorf("unexpected conversation identity: session=%q harness=%q mode=%q", snapshot.SessionID, snapshot.Harness, snapshot.Mode)
	}
	if snapshot.Settings.Model != fixtureAgentModel || snapshot.Settings.ReasoningEffort != fixtureAgentEffort || (snapshot.Settings.ApprovalMode != "" && snapshot.Settings.ApprovalMode != string(ports.PermissionModeDefault)) {
		return false, snapshot.Controller, errors.New("conversation read model does not preserve the fixture model, Medium effort and default permissions")
	}
	switch ports.ChatControllerState(snapshot.Controller) {
	case ports.ChatControllerConnecting, ports.ChatControllerRecovering, ports.ChatControllerStopped:
		return false, snapshot.Controller, nil
	case ports.ChatControllerReady:
		caps := ports.ChatCapabilities{}
		for _, capability := range snapshot.Capabilities {
			caps[ports.ChatCapability(capability)] = true
		}
		if missing := ports.MissingCapabilitiesForPermissions(caps, ports.PermissionModeDefault); len(missing) != 0 {
			return false, snapshot.Controller, fmt.Errorf("ready conversation is missing capabilities: %v", missing)
		}
		if len(snapshot.Turns) != 0 {
			return false, snapshot.Controller, errors.New("pre-send fixture conversation unexpectedly contains turns")
		}
		return true, snapshot.Controller, nil
	default:
		return false, snapshot.Controller, fmt.Errorf("unexpected pre-send controller state %q", snapshot.Controller)
	}
}

func TestFixtureConversationReadinessRequiresAdmittedController(t *testing.T) {
	snapshot := controllers.ConversationSnapshotResponse{
		SessionID: "fixture-project-2", Harness: string(domain.HarnessCodex), Mode: string(domain.SessionModeChat),
		Controller:   string(ports.ChatControllerReady),
		Settings:     controllers.ConversationTurnSettingsPayload{Model: fixtureAgentModel, ReasoningEffort: fixtureAgentEffort},
		Capabilities: []string{"streaming", "approvals", "resume", "interrupt"},
	}
	for _, test := range []struct {
		name             string
		change           func(*controllers.ConversationSnapshotResponse)
		ready, wantError bool
	}{
		{"ready", func(*controllers.ConversationSnapshotResponse) {}, true, false},
		{"connecting", func(s *controllers.ConversationSnapshotResponse) { s.Controller = "connecting" }, false, false},
		{"stopped", func(s *controllers.ConversationSnapshotResponse) { s.Controller = "stopped" }, false, false},
		{"missing approval", func(s *controllers.ConversationSnapshotResponse) {
			s.Capabilities = []string{"streaming", "resume", "interrupt"}
		}, false, true},
		{"wrong session", func(s *controllers.ConversationSnapshotResponse) { s.SessionID = "foreign" }, false, true},
		{"high effort", func(s *controllers.ConversationSnapshotResponse) { s.Settings.ReasoningEffort = "high" }, false, true},
		{"bypass permissions", func(s *controllers.ConversationSnapshotResponse) { s.Settings.ApprovalMode = "bypass-permissions" }, false, true},
		{"unexpected busy turn", func(s *controllers.ConversationSnapshotResponse) { s.Controller = "busy" }, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := snapshot
			test.change(&current)
			body, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			ready, _, err := fixtureConversationReady(body, snapshot.SessionID)
			if ready != test.ready || (err != nil) != test.wantError {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestFixtureChatClosePreventsDispatchBeforeGuardOrCallback(t *testing.T) {
	effects := &fixtureEffectRecorder{}
	conversation := newFixtureApprovalConversation(effects)
	// A detached fixture must reject before opening any policy/authority or
	// invoking the write callback; this conversation deliberately has no gate.
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.SendTurn(context.Background(), ports.ChatUserMessage{ClientMessageID: "fixture-complete"}); err == nil {
		t.Fatal("detached conversation accepted a turn")
	}
	if len(effects.entries) != 2 ||
		effects.entries[0].Name != "chat.controller-detach" ||
		effects.entries[1].Kind != "forbidden" ||
		effects.entries[1].Name != "chat.detached-provider-write" {
		t.Fatalf("detached send crossed the fixture boundary: %+v", effects.entries)
	}
}
