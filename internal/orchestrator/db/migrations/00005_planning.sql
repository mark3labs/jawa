-- +goose Up
CREATE TABLE planning_sessions (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'queued', repo TEXT NOT NULL, base_branch TEXT NOT NULL, provider TEXT NOT NULL,
 title TEXT NOT NULL, description TEXT NOT NULL, issue_url TEXT NOT NULL,
 repo_commit TEXT NOT NULL DEFAULT '', spec TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0,
 approved_revision INTEGER NOT NULL DEFAULT 0, approved_spec TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX planning_active_card ON planning_sessions(card_id) WHERE state NOT IN ('cancelled','approved');
CREATE TABLE planning_messages (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES planning_sessions(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, created_at INTEGER NOT NULL,
 UNIQUE(session_id,sequence));
CREATE TABLE planning_revisions (
 session_id TEXT NOT NULL REFERENCES planning_sessions(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, spec TEXT NOT NULL, questions_json TEXT NOT NULL, repo_commit TEXT NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(session_id,number));
CREATE TABLE planning_turns (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES planning_sessions(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'queued', task_json TEXT NOT NULL,
 published INTEGER NOT NULL DEFAULT 0, cancel_sent INTEGER NOT NULL DEFAULT 0, agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
 remote_attempt_id TEXT NOT NULL DEFAULT '', event_seq INTEGER NOT NULL DEFAULT -1,
 result TEXT NOT NULL DEFAULT '', outcome_json TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(session_id,number));
CREATE TABLE planning_deliveries (
 id INTEGER PRIMARY KEY, turn_id TEXT NOT NULL REFERENCES planning_turns(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, payload TEXT NOT NULL, received_at INTEGER NOT NULL,
 UNIQUE(turn_id,kind,payload));
-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
