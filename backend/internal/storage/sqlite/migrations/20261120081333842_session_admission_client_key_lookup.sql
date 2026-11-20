-- +goose Up
-- Keep legacy collisions and malformed payloads available for reconciliation.
-- Nonunique lookup enforces global client-key scope without changing command IDs.
CREATE INDEX session_admission_spawn_client_project
ON session_admission_queue(json_extract(payload,'$.ClientRequestID'), project_id)
WHERE operation='spawn' AND json_valid(payload);

-- +goose Down
DROP INDEX session_admission_spawn_client_project;
