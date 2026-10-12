-- sqlc schema snapshot; keep in sync with migrations (including Go upgrades).
CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK(id=1), username TEXT NOT NULL DEFAULT 'admin', password_hash TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL, provider TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', base_branch TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS cards (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('Todo','Planning','Building','Fix','Done')), position INTEGER NOT NULL CHECK(position>=0), created_at INTEGER NOT NULL, issue_url TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workflow_attempts (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, task_json TEXT NOT NULL,
 agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', remote_attempt_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'queued', run_state TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
 outcome_json TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
 published INTEGER NOT NULL DEFAULT 0, ready INTEGER NOT NULL DEFAULT 0, event_seq INTEGER NOT NULL DEFAULT -1,
 branch TEXT NOT NULL DEFAULT '', repair_message TEXT NOT NULL DEFAULT '', repair_pr_url TEXT NOT NULL DEFAULT '',
 repair_source_number INTEGER NOT NULL DEFAULT 0,
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

-- Independent of card cascades: a lost ACK remains replayable after deletion.
CREATE TABLE typed_reports (
 report_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, tool_call_id TEXT NOT NULL,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 turn_id TEXT NOT NULL, kind TEXT NOT NULL, request_json TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 UNIQUE(task_id,tool_call_id));
CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind IN ('complete_task','report_blocker');
CREATE TABLE planning_interactions (
 report_id TEXT PRIMARY KEY REFERENCES typed_reports(report_id),
 task_id TEXT NOT NULL REFERENCES planning_turns(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','answered','cancelled')),
 waiting_json TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '', approved INTEGER,
 created_at INTEGER NOT NULL, answered_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX planning_pending_interaction ON planning_interactions(task_id) WHERE status IN ('pending','queued');
CREATE TABLE IF NOT EXISTS workflow_cancel_requests (
 id INTEGER PRIMARY KEY, task_id TEXT, agent_id TEXT, run_id TEXT, remote_attempt_id TEXT, turn_id TEXT,
 status TEXT, error TEXT, created_at INTEGER);

CREATE TABLE failed_resumes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 planning INTEGER NOT NULL, request_json TEXT NOT NULL, previous_outcome TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', reply_json TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE UNIQUE INDEX failed_resume_pending ON failed_resumes(task_id) WHERE status='pending';

CREATE TABLE workflow_pr_checks (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL,
 provider TEXT NOT NULL,
 repo TEXT NOT NULL,
 base_branch TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('conflict','clear','unknown','error','merged','closed','checking')),
 head_sha TEXT NOT NULL DEFAULT '',
 base_sha TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL
);
