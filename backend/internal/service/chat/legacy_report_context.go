package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// splitLegacyWorkerReports recognizes only the exact appended envelope emitted
// by older AO versions. New sends never use this representation. Preserve it as
// hidden content before editing/retrying, so upgrading does not discard already
// acknowledged context or copy it into a newly authored message.
func splitLegacyWorkerReports(text string) (string, string) {
	const open = "\n\n<ao-worker-reports>\n"
	const close = "\n</ao-worker-reports>"
	index := strings.Index(text, open)
	if index < 0 || !strings.HasSuffix(text, close) {
		return text, ""
	}
	return text[:index], text[index+2:]
}

func retainLegacyWorkerReports(content []ports.ChatContent, context string) []ports.ChatContent {
	if context == "" {
		return content
	}
	for _, block := range content {
		if block.Internal && block.Type == "text" && block.Text == context {
			return content
		}
	}
	digest := sha256.Sum256([]byte(context))
	return append(content, ports.ChatContent{Type: "text", Internal: true, URI: "ao://legacy-worker-reports/" + hex.EncodeToString(digest[:16]), Text: context})
}
