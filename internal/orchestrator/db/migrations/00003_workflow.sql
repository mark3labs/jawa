-- +goose Up
CREATE TABLE IF NOT EXISTS workflow_attempts (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, task_json TEXT NOT NULL,
 agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', remote_attempt_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'queued', run_state TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
 outcome_json TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
 published INTEGER NOT NULL DEFAULT 0, ready INTEGER NOT NULL DEFAULT 0, event_seq INTEGER NOT NULL DEFAULT -1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(card_id,number));
 CREATE TABLE IF NOT EXISTS workflow_conflicts (
 id INTEGER PRIMARY KEY, task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, remote_attempt_id TEXT NOT NULL,
 outcome_json TEXT NOT NULL, received_at INTEGER NOT NULL, reason TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS workflow_conflicts_identity ON workflow_conflicts(task_id,run_id,remote_attempt_id);
 CREATE TABLE IF NOT EXISTS workflow_execution_events (
 task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 event_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, remote_attempt_id TEXT NOT NULL,
 event_type TEXT NOT NULL, event_json TEXT NOT NULL, received_at INTEGER NOT NULL,
 PRIMARY KEY(task_id,event_id));
 CREATE TABLE IF NOT EXISTS workflow_reconciliations (
 id INTEGER PRIMARY KEY, task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 previous_agent_id TEXT NOT NULL, previous_run_id TEXT NOT NULL, previous_remote_attempt_id TEXT NOT NULL,
 selected_agent_id TEXT NOT NULL, selected_run_id TEXT NOT NULL, selected_remote_attempt_id TEXT NOT NULL,
 conflict_id INTEGER NOT NULL REFERENCES workflow_conflicts(id) ON DELETE CASCADE,
 reconciled_at INTEGER NOT NULL);
