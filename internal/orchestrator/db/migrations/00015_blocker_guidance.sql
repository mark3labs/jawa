-- +goose Up
-- A blocker committed as awaiting guidance parks its run instead of ending it,
-- so a task may report several blockers. Completion stays one-per-task.
-- Blockers without an interaction row (legacy or post-approval) stay terminal.
DROP INDEX typed_reports_terminal;
CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind='complete_task';
CREATE TABLE workflow_interactions (
 report_id TEXT PRIMARY KEY REFERENCES typed_reports(report_id),
 task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','answered','cancelled')),
 waiting_json TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, answered_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX workflow_pending_interaction ON workflow_interactions(task_id) WHERE status IN ('pending','queued');
-- +goose Down
SELECT 1;
