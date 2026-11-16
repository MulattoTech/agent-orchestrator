-- name: ClaimReviewPublicationAttempt :execrows
INSERT INTO review_publication_attempt
    (run_id, session_id, pr_url, target_sha, verdict, body, claim_id,
     state, attempt_count, attempted_at)
SELECT sqlc.arg(run_id), sqlc.arg(session_id), sqlc.arg(pr_url),
       sqlc.arg(target_sha), sqlc.arg(verdict), sqlc.arg(body),
       sqlc.arg(claim_id), 'uncertain', 1, sqlc.arg(attempted_at)
FROM review_run
WHERE id = sqlc.arg(run_id) AND session_id = sqlc.arg(session_id)
  AND pr_url = sqlc.arg(pr_url) AND target_sha = sqlc.arg(target_sha)
  AND verdict = sqlc.arg(verdict) AND body = sqlc.arg(body)
  AND status IN ('complete', 'delivered')
ON CONFLICT (run_id) DO UPDATE SET
    claim_id = excluded.claim_id, state = 'uncertain',
    attempt_count = review_publication_attempt.attempt_count + 1,
    attempted_at = excluded.attempted_at
WHERE review_publication_attempt.state = 'ready'
  AND review_publication_attempt.provider_review_id = ''
  AND review_publication_attempt.session_id = excluded.session_id
  AND review_publication_attempt.pr_url = excluded.pr_url
  AND review_publication_attempt.target_sha = excluded.target_sha
  AND review_publication_attempt.verdict = excluded.verdict
  AND review_publication_attempt.body = excluded.body;

-- name: GetReviewPublicationAttempt :one
SELECT * FROM review_publication_attempt WHERE run_id = ?;

-- name: ReleaseReviewPublicationPrewrite :execrows
UPDATE review_publication_attempt SET state = 'ready'
WHERE run_id = sqlc.arg(run_id) AND claim_id = sqlc.arg(claim_id)
  AND state = 'uncertain' AND provider_review_id = '';

-- name: CompleteReviewPublicationAttempt :execrows
UPDATE review_publication_attempt
SET state = 'published', provider_review_id = sqlc.arg(provider_review_id),
    provider_state = sqlc.arg(provider_state), provider_url = sqlc.arg(provider_url),
    completed_at = COALESCE(completed_at, sqlc.arg(completed_at))
WHERE run_id = sqlc.arg(run_id) AND claim_id = sqlc.arg(claim_id)
  AND (provider_review_id = '' OR provider_review_id = sqlc.arg(provider_review_id))
  AND (state != 'published' OR
       (provider_state = sqlc.arg(provider_state) AND provider_url = sqlc.arg(provider_url)));
