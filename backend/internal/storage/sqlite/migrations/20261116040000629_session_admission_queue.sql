-- +goose Up
CREATE TABLE session_admission_queue (
 id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 operation TEXT NOT NULL CHECK(operation IN ('spawn','restore')),
 session_id TEXT NOT NULL DEFAULT '',
 payload TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','running','completed','blocked','cancelled')),
 generation INTEGER NOT NULL DEFAULT 0,
 owner TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0,
 failure_code TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 available_at TEXT NOT NULL
);
CREATE INDEX session_admission_queue_due ON session_admission_queue(state, available_at, created_at);
CREATE TABLE session_admission_settings (
 id INTEGER PRIMARY KEY CHECK (id=1),
 limits_json TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
-- +goose Down
DROP TABLE session_admission_settings;
DROP TABLE session_admission_queue;
