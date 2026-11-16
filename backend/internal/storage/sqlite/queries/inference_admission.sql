-- Return only a provably unsubmitted turn to its original durable queue position.
-- Callers may use this only for a typed final-boundary refusal before any write.
-- name: RequeueUndispatchedConversationTurn :execrows
UPDATE conversation_turns
SET state = 'queued', started_at = NULL, promotion_started_at = NULL,
    error_message = sqlc.arg(reason), provider_input_text = ''
WHERE id = sqlc.arg(id)
  AND conversation_id = sqlc.arg(conversation_id)
  AND state IN ('queued', 'running')
  AND provider_turn_id = ''
  AND completed_at IS NULL;
