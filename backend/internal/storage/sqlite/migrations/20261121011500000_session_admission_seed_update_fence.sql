-- +goose Up
-- A speculative task preparation is INSERTed with no client key and later
-- promoted by UPDATE sessions.client_request_id/client_request_hash.
-- The earlier INSERT fence cannot see that later ownership change. Protect
-- key-changing UPDATEs with the same atomic SQLite writer/queued-key test.
-- Do not modify existing rows or remove historical conflicting queue entries.
-- A matching already-RUNNING queued dispatcher is allowed to promote its own
-- task preparation; cancelled/blocked/queued/foreign/hash-mismatch claims are
-- never proof of seed ownership.
-- +goose StatementBegin
CREATE TRIGGER session_admission_client_key_seed_update_fence
BEFORE UPDATE OF client_request_id, client_request_hash ON sessions
WHEN COALESCE(NEW.client_request_id, '') <> ''
 AND (
     COALESCE(NEW.client_request_id, '') <> COALESCE(OLD.client_request_id, '')
     OR COALESCE(NEW.client_request_hash, '') <> COALESCE(OLD.client_request_hash, '')
 )
 AND EXISTS (
     SELECT 1 FROM session_admission_queue AS q
     WHERE q.operation = 'spawn' AND json_valid(q.payload)
       AND json_extract(q.payload, '$.ClientRequestID') = NEW.client_request_id
       AND (
           q.state <> 'running'
           OR COALESCE(q.project_id, '') <> COALESCE(NEW.project_id, '')
           OR COALESCE(json_extract(q.payload, '$.ClientRequestHash'), '')
                <> COALESCE(NEW.client_request_hash, '')
       )
 )
BEGIN
    SELECT RAISE(ABORT, 'admission queued key conflicts with session seed');
END;
-- +goose StatementEnd

-- +goose Down
-- CAUTION: Down removes UPDATE-side protection. Do not run on a live AO
-- database without separately authorized writer freeze and recovery plan.
DROP TRIGGER session_admission_client_key_seed_update_fence;
