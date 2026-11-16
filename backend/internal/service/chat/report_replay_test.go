package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReportContextNativeHistoryMatchesDispatchedPrompt(t *testing.T) {
	context := "<ao-worker-reports>\nfixture\n</ao-worker-reports>"
	blocks := []ports.ChatContent{{Type: "text", Internal: true, Text: context, URI: ports.ChatWorkerReportURIPrefix + "batch"}}
	encoded, _ := json.Marshal(blocks)
	message := domain.ConversationMessage{ID: "report-user", TurnID: "ao-turn", Role: domain.MessageRoleUser, Text: "continue", DeliveryContentJSON: string(encoded)}
	want := "continue\n\n" + context
	if got := nativeHistoryMessageText(message); got != want {
		t.Fatalf("match text = %q", got)
	}
	index := indexNativeHistoryTurns([]domain.ConversationTurn{{ID: "ao-turn", ProviderTurnID: "old", State: domain.TurnStateCompleted}}, []domain.ConversationMessage{message}, nil)
	mapped, _ := index.mapReplay([]ports.ChatEvent{{Kind: ports.ChatEventUserMessageCompleted, ProviderTurnID: "new", Text: want}})
	if mapped["new"] == nil || mapped["new"].providerTurnID != "old" {
		t.Fatal("native reload duplicated report-bearing turn")
	}
	if strings.Contains(message.Text, "<ao-worker-reports>") {
		t.Fatal("history match mutated authored text")
	}
}

func TestRetryContentRemovesOnlyPreviouslyRenderedBlocks(t *testing.T) {
	blocks := []ports.ChatContent{
		{Type: "text", Internal: true, Text: "report"},
		{Type: "image", Data: "fixture"},
		{Type: "resource", URI: "file:///readme", Text: "retained"},
	}
	got := unrenderedRetryContent(blocks)
	if len(got) != 2 || got[0].Type != "image" || got[1].Text != "retained" {
		t.Fatalf("native content changed: %+v", got)
	}
	if len(blocks) != 3 || blocks[0].Text != "report" {
		t.Fatal("durable content mutated")
	}
}
