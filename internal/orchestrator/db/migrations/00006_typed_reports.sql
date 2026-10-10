-- +goose Up
-- Independent of card cascades: a lost ACK remains replayable after deletion.
CREATE TABLE typed_reports (
 task_id TEXT PRIMARY KEY,
 report_id TEXT NOT NULL UNIQUE,
 agent_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 turn_id TEXT NOT NULL,
 kind TEXT NOT NULL,
 request_json TEXT NOT NULL,
 accepted_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS workflow_cancel_requests (
 id INTEGER PRIMARY KEY, task_id TEXT, agent_id TEXT, run_id TEXT, remote_attempt_id TEXT, turn_id TEXT,
 status TEXT, error TEXT, created_at INTEGER);
-- +goose Down
DROP TABLE typed_reports;
