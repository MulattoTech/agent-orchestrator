-- +goose Up
-- The first durable claim is the only authority to make a publication-capable
-- call. An uncertain claim never expires; a missing remote marker is not proof
-- that an earlier POST was rejected.
CREATE TABLE review_publication_attempt (
    run_id TEXT PRIMARY KEY REFERENCES review_run(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    pr_url TEXT NOT NULL,
    target_sha TEXT NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('approved', 'changes_requested')),
    body TEXT NOT NULL,
    claim_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('ready', 'uncertain', 'published')),
    attempt_count INTEGER NOT NULL DEFAULT 1,
    attempted_at TIMESTAMP NOT NULL,
    provider_review_id TEXT NOT NULL DEFAULT '',
    provider_state TEXT NOT NULL DEFAULT '',
    provider_url TEXT NOT NULL DEFAULT '',
    completed_at TIMESTAMP
);

-- Older completed runs have no durable proof that publication never started.
-- Preserve them as reconciliation-only, including legacy receipts. Never infer
-- permission to POST from the absence of an old local receipt.
INSERT INTO review_publication_attempt
    (run_id, session_id, pr_url, target_sha, verdict, body, claim_id,
     state, attempt_count, attempted_at, provider_review_id)
SELECT id, session_id, pr_url, target_sha, verdict, body, 'legacy:' || id,
       'uncertain', 0, created_at, github_review_id
FROM review_run
WHERE status IN ('complete', 'delivered')
  AND verdict IN ('approved', 'changes_requested') AND body != '';

-- +goose Down
DROP TABLE review_publication_attempt;
