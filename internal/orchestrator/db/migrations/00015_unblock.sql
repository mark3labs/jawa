-- +goose Up
ALTER TABLE workflow_attempts ADD COLUMN report_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_attempts ADD COLUMN report_turn TEXT NOT NULL DEFAULT '';
ALTER TABLE typed_reports ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE typed_reports ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';
DROP INDEX typed_reports_terminal;
CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id,generation) WHERE kind IN ('complete_task','report_blocker');
ALTER TABLE failed_resumes ADD COLUMN mode TEXT NOT NULL DEFAULT 'failed';
ALTER TABLE failed_resumes ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE failed_resumes ADD COLUMN previous_snapshot TEXT NOT NULL DEFAULT '';
ALTER TABLE failed_resumes ADD COLUMN blocker_report_id TEXT NOT NULL DEFAULT '';
CREATE TABLE workflow_ready_overrides (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL, generation INTEGER NOT NULL, action_token TEXT NOT NULL,
 prior_snapshot TEXT NOT NULL, operator TEXT NOT NULL, marked_at INTEGER NOT NULL);
