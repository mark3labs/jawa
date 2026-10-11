-- +goose Up
-- Receipts survive card deletion and retain legacy terminal identities.
ALTER TABLE typed_reports RENAME TO typed_reports_v6;
CREATE TABLE typed_reports (
 report_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, tool_call_id TEXT NOT NULL,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 turn_id TEXT NOT NULL, kind TEXT NOT NULL, request_json TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 UNIQUE(task_id,tool_call_id));
INSERT INTO typed_reports SELECT report_id,task_id,'legacy:' || report_id,agent_id,run_id,attempt_id,turn_id,kind,request_json,accepted_at FROM typed_reports_v6;
DROP TABLE typed_reports_v6;
CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind IN ('complete_task','report_blocker');
CREATE TABLE planning_interactions (
 report_id TEXT PRIMARY KEY REFERENCES typed_reports(report_id),
 task_id TEXT NOT NULL REFERENCES planning_turns(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','answered','cancelled')),
 waiting_json TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '', approved INTEGER,
 created_at INTEGER NOT NULL, answered_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX planning_pending_interaction ON planning_interactions(task_id) WHERE status IN ('pending','queued');

-- v6 proposals ended their remote run; they cannot be answered as suspensions.
-- Keep history/specs intact but require an explicit new investigation on retry.
UPDATE planning_sessions SET state='failed',error='Legacy planner completed without a resumable interaction; retry planning to continue.' WHERE state IN ('ready','questions');
