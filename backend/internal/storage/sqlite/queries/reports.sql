-- name: CreateReport :one
INSERT INTO reports (
    id, session_id, project_id, state, note, message, created_at, available_at,
    settlement_deadline, repeat_count
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: CreateReportOutput :exec
INSERT INTO report_outputs (report_id, position, kind, reference, label)
VALUES (?, ?, ?, ?, ?);

-- name: GetReport :one
SELECT * FROM reports WHERE id = ?;

-- name: ListReportOutputs :many
SELECT * FROM report_outputs WHERE report_id = ? ORDER BY position;

-- name: ListReportsBySession :many
SELECT * FROM reports WHERE session_id = ? ORDER BY created_at, id;

-- name: ListReportedPRURLs :many
SELECT o.reference FROM report_outputs o
JOIN reports r ON r.id = o.report_id
WHERE r.session_id = ? AND o.kind = 'pr_created'
ORDER BY r.created_at, r.id, o.position;

-- name: ListReportsByProject :many
SELECT * FROM reports WHERE project_id = ? ORDER BY created_at, id;

-- name: ListPendingReportSchedule :many
SELECT * FROM reports
WHERE delivery_state = 'pending'
ORDER BY available_at, created_at, id
LIMIT ?;

-- name: ListPendingReportsByProjectSchedule :many
SELECT * FROM reports
WHERE project_id = ? AND delivery_state = 'pending'
ORDER BY available_at, created_at, id
LIMIT ?;

-- name: AssignPendingReportsToBatch :exec
UPDATE reports
SET delivery_batch_id = sqlc.arg(delivery_batch_id)
WHERE project_id = sqlc.arg(project_id) AND delivery_state = 'pending'
  AND delivery_batch_id = '';

-- name: ClaimPendingReportsByBatch :many
UPDATE reports
SET delivery_state = 'claimed', claim_token = sqlc.arg(claim_token),
    claimed_at = sqlc.arg(claimed_at), delivery_attempts = delivery_attempts + 1,
    last_error = ''
WHERE project_id = sqlc.arg(project_id) AND delivery_state = 'pending'
  AND delivery_batch_id = sqlc.arg(delivery_batch_id)
RETURNING *;

-- name: AcknowledgeReportBatch :many
UPDATE reports
SET delivery_state = 'acknowledged', acknowledged_at = sqlc.arg(acknowledged_at)
WHERE project_id = sqlc.arg(project_id) AND delivery_state = 'claimed'
  AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: ReleaseReportBatch :many
UPDATE reports
SET delivery_state = 'pending', available_at = min(available_at, sqlc.arg(available_at)),
    claim_token = '', claimed_at = NULL, last_error = sqlc.arg(last_error)
WHERE project_id = sqlc.arg(project_id) AND delivery_state = 'claimed'
  AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: DeferReportBatch :many
UPDATE reports
SET delivery_state = 'pending', available_at = max(available_at, sqlc.arg(available_at)),
    claim_token = '', claimed_at = NULL, last_error = sqlc.arg(last_error)
WHERE project_id = sqlc.arg(project_id) AND delivery_state = 'claimed'
  AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: GetReportInterrupt :one
SELECT last_interrupted_at FROM report_worker_interrupts WHERE session_id = ?;

-- name: PutReportInterrupt :exec
INSERT INTO report_worker_interrupts (session_id, last_interrupted_at)
VALUES (?, ?)
ON CONFLICT(session_id) DO UPDATE SET last_interrupted_at = excluded.last_interrupted_at;

-- name: ListPendingReports :many
SELECT * FROM reports
WHERE delivery_state = 'pending' AND available_at <= ?
ORDER BY created_at, id
LIMIT ?;

-- name: ClaimReport :one
UPDATE reports
SET delivery_state = 'claimed', claim_token = sqlc.arg(claim_token),
    claimed_at = sqlc.arg(claimed_at), delivery_attempts = delivery_attempts + 1,
    last_error = ''
WHERE id = sqlc.arg(id) AND delivery_state = 'pending' AND available_at <= sqlc.arg(claimed_at)
RETURNING *;

-- name: AcknowledgeReport :one
UPDATE reports
SET delivery_state = 'acknowledged', acknowledged_at = sqlc.arg(acknowledged_at)
WHERE id = sqlc.arg(id) AND delivery_state = 'claimed' AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: ReleaseReport :one
UPDATE reports
SET delivery_state = 'pending', available_at = sqlc.arg(available_at),
    claim_token = '', claimed_at = NULL, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND delivery_state = 'claimed' AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- A crash can happen after a durable message append but before report ACK.
-- Recover that acceptance from its exact batch identity, within the same project,
-- instead of attaching a human piggyback to a second message after restart.
-- Undispatched withdrawn/rolled-back context is not an accepted delivery.
-- name: RecoverAcceptedReportClaims :execrows
UPDATE reports
SET delivery_state = 'acknowledged', acknowledged_at = sqlc.arg(acknowledged_at),
    last_error = 'durable message acceptance recovered after daemon restart'
WHERE reports.delivery_state = 'claimed' AND reports.delivery_batch_id <> ''
  AND EXISTS (
      SELECT 1
      FROM conversation_messages AS message
      JOIN conversation_turns AS turn ON turn.id = message.turn_id
      JOIN conversations AS conversation ON conversation.id = message.conversation_id
      WHERE conversation.project_id = reports.project_id
        AND turn.conversation_id = message.conversation_id
        AND message.role = 'user'
        AND turn.state <> 'cancelled'
        AND (turn.state <> 'interrupted' OR turn.provider_turn_id <> ''
             OR turn.provider_input_text <> '' OR turn.promotion_started_at IS NOT NULL)
        AND turn.rolled_back_at IS NULL
        AND (
            (message.origin = 'automation' AND message.client_message_id = reports.delivery_batch_id)
            OR (message.origin = 'human' AND EXISTS (
                SELECT 1
                FROM json_each(CASE WHEN json_valid(message.delivery_content_json)
                    THEN message.delivery_content_json ELSE '[]' END) AS block
                WHERE json_extract(CASE WHEN block.type = 'object' THEN block.value ELSE '{}' END, '$.type') = 'text'
                  AND json_extract(CASE WHEN block.type = 'object' THEN block.value ELSE '{}' END, '$.internal') = 1
                  AND json_extract(CASE WHEN block.type = 'object' THEN block.value ELSE '{}' END, '$.uri') = 'ao://worker-reports/' || reports.delivery_batch_id
                  AND COALESCE(json_extract(CASE WHEN block.type = 'object' THEN block.value ELSE '{}' END, '$.text'), '') <> ''
            ))
        )
  );

-- name: RequeueClaimedReports :execrows
UPDATE reports
SET delivery_state = 'pending', claim_token = '', claimed_at = NULL,
    last_error = 'delivery claim recovered after daemon restart'
WHERE delivery_state = 'claimed';

-- A human can withdraw an undispatched queue item after its report batch was
-- acknowledged as durably accepted. Restore that batch atomically with cancel.
-- The conversation's project fences a forged/corrupt cross-project reference.
-- name: RequeueUndispatchedReportBatch :exec
UPDATE reports
SET delivery_state = 'pending', claim_token = '', claimed_at = NULL,
    acknowledged_at = NULL, available_at = min(available_at, sqlc.arg(available_at)),
    last_error = 'accepted report context withdrawn before provider dispatch'
WHERE delivery_state = 'acknowledged'
  AND delivery_batch_id = sqlc.arg(delivery_batch_id)
  AND reports.project_id = (SELECT conversations.project_id FROM conversations WHERE conversations.id = sqlc.arg(conversation_id));
