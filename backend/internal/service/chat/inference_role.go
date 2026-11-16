package chat

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// inferenceRole is derived from durable controller ownership, never user text.
func inferenceRole(cfg StartConfig) string {
	if cfg.Owner.Kind == domain.ConversationOwnerReview || cfg.ReadOnly {
		return "reviewer"
	}
	if cfg.Kind == domain.KindOrchestrator {
		return "orchestrator"
	}
	return "implementation"
}
