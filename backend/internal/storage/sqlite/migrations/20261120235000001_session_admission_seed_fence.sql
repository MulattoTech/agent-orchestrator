-- +goose Up
-- The session admission queue and the client session table are separate, so
-- their individual unique indexes cannot arbitrate a key across both tables.
-- A SQLite BEFORE INSERT trigger shares the database writer lock with the
-- durable session insertion, including writes from independent Store objects.
-- Old, including conflicting, rows are left in place for reconciliation.
--
-- A dispatcher is permitted to create the session for its OWN running
-- queued command, if the project and client request hash both match.
-- A paused or terminal command, another project, or a different hash is not
-- proof that an independent seed may take ownership of the client key.
-- +goose StatementBegin
CREATE TRIGGER session_admission_client_key_seed_fence
BEFORE INSERT ON sessions
WHEN NEW.client_request_id <> ''
 AND EXISTS (
   SELECT 1 FROM session_admission_queue AS q
   WHERE q.operation = 'spawn' AND json_valid(q.payload)
     AND json_extract(q.payload,'$.ClientRequestID') = NEW.client_request_id
     AND (
       q.state <> 'running'
       OR COALESCE(q.project_id,'') <> COALESCE(NEW.project_id,'')
       OR COALESCE(json_extract(q.payload,'$.ClientRequestHash'),'') <> NEW.client_request_hash
     )
 )
BEGIN
 SELECT RAISE(ABORT, 'admission queued key conflicts with session seed');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER session_admission_client_key_seed_fence;
