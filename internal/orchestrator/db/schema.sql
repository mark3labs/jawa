-- sqlc schema snapshot; keep in sync with migrations (including Go upgrades).
CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK(id=1), username TEXT NOT NULL DEFAULT 'admin', password_hash TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL, provider TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', base_branch TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS cards (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('Todo','Planning','Building','Fix','Done')), position INTEGER NOT NULL CHECK(position>=0), created_at INTEGER NOT NULL, issue_url TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)), auto_merge_mode TEXT NOT NULL DEFAULT 'inherit' CHECK(auto_merge_mode IN ('inherit','on','off')));
 CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workflow_attempts (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, task_json TEXT NOT NULL,
 target_agent_id TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', remote_attempt_id TEXT NOT NULL DEFAULT '',
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
 target_agent_id TEXT NOT NULL DEFAULT '', published INTEGER NOT NULL DEFAULT 0, cancel_sent INTEGER NOT NULL DEFAULT 0, agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
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
CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind='complete_task';
CREATE TABLE planning_interactions (
 report_id TEXT PRIMARY KEY REFERENCES typed_reports(report_id),
 task_id TEXT NOT NULL REFERENCES planning_turns(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','answered','cancelled')),
 waiting_json TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '', approved INTEGER,
 created_at INTEGER NOT NULL, answered_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX planning_pending_interaction ON planning_interactions(task_id) WHERE status IN ('pending','queued');
CREATE TABLE workflow_interactions (
 report_id TEXT PRIMARY KEY REFERENCES typed_reports(report_id),
 task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','answered','cancelled')),
 waiting_json TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, answered_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX workflow_pending_interaction ON workflow_interactions(task_id) WHERE status IN ('pending','queued');
CREATE TABLE IF NOT EXISTS workflow_cancel_requests (
 id INTEGER PRIMARY KEY, task_id TEXT, agent_id TEXT, run_id TEXT, remote_attempt_id TEXT, turn_id TEXT,
 status TEXT, error TEXT, created_at INTEGER);

CREATE TABLE failed_resumes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 planning INTEGER NOT NULL, request_json TEXT NOT NULL, previous_outcome TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', reply_json TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 automatic INTEGER NOT NULL DEFAULT 0 CHECK(automatic IN (0,1)));
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

CREATE TABLE auto_resumes (
 task_id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 planning INTEGER NOT NULL CHECK(planning IN (0,1)), failure_token TEXT NOT NULL,
 due_at INTEGER NOT NULL, next_check_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('scheduled','reserved','stopped','exhausted')),
 triage TEXT NOT NULL DEFAULT '' CHECK(triage IN ('','transient','permanent','unavailable')),
 triage_probability REAL NOT NULL DEFAULT 0);
CREATE INDEX auto_resume_due ON auto_resumes(state,next_check_at,task_id);
CREATE TABLE workflow_pr_merge_overrides (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL, provider TEXT NOT NULL, repo TEXT NOT NULL,
 base_branch TEXT NOT NULL, marked_at INTEGER NOT NULL);
CREATE TABLE issue_submissions (
 token TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 provider TEXT NOT NULL,
 repository TEXT NOT NULL,
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 auto_merge_mode TEXT NOT NULL DEFAULT 'inherit' CHECK(auto_merge_mode IN ('inherit','on','off')),
 state TEXT NOT NULL CHECK(state IN ('reserved','rejected','uncertain','created','completed')),
 issue_url TEXT NOT NULL DEFAULT '',
 issue_number INTEGER NOT NULL DEFAULT 0,
 card_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
-- Deliberately no cascading foreign keys: deletion must not erase replay evidence.
CREATE TABLE workflow_ready_overrides (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 action_token TEXT NOT NULL,
 prior_snapshot TEXT NOT NULL,
 operator TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 marked_at INTEGER NOT NULL
);

-- No foreign keys: uncertainty and completed-operation tombstones survive deletion.
CREATE TABLE auto_merge_operations (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 provider TEXT NOT NULL, repository TEXT NOT NULL, base_branch TEXT NOT NULL,
 pr_url TEXT NOT NULL, head_sha TEXT NOT NULL,
 config_token TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('reserved','inflight','uncertain','deferred','rejected','merged')),
 category TEXT NOT NULL DEFAULT '', retries INTEGER NOT NULL DEFAULT 0,
 next_check_at INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(attempt_id,provider,repository,base_branch,pr_url,head_sha)
);
CREATE INDEX auto_merge_due ON auto_merge_operations(next_check_at,attempt_id);
-- Reservation fences are shared by all workflow and archive mutations.
CREATE TRIGGER auto_merge_card_update BEFORE UPDATE OF status,archived,project_id ON cards
WHEN EXISTS(SELECT 1 FROM auto_merge_operations WHERE card_id=OLD.id AND state IN ('reserved','inflight'))
BEGIN SELECT RAISE(ABORT,'external merge reserved; wait for reconciliation'); END;
CREATE TRIGGER auto_merge_card_delete BEFORE DELETE ON cards
WHEN EXISTS(SELECT 1 FROM auto_merge_operations WHERE card_id=OLD.id AND state IN ('reserved','inflight'))
BEGIN SELECT RAISE(ABORT,'external merge reserved; wait for reconciliation'); END;
CREATE TABLE pr_merges (
 id TEXT PRIMARY KEY,
 card_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 pr_url TEXT NOT NULL,
 snapshot TEXT NOT NULL,
 provider TEXT NOT NULL,
 repo TEXT NOT NULL,
 base_branch TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 method TEXT NOT NULL CHECK(method IN ('merge','squash','rebase')),
 operator TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','merged','rejected','uncertain')),
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX pr_merges_unresolved ON pr_merges(card_id) WHERE state IN ('pending','uncertain');
-- Retain replay/audit evidence even after local card deletion.
CREATE TABLE run_chat (
 id TEXT PRIMARY KEY,
 card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 mode TEXT NOT NULL CHECK(mode IN ('Planning','Build','Fix')),
 local_id TEXT NOT NULL, client_id TEXT NOT NULL,
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 text TEXT NOT NULL, classification TEXT NOT NULL,
 interaction TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','dispatched','active','completed','failed','ambiguous')),
 failure TEXT NOT NULL DEFAULT '', request_json TEXT NOT NULL DEFAULT '', receipt_json TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(mode,local_id,client_id)
);
CREATE INDEX run_chat_queue ON run_chat(status,created_at,id);
CREATE INDEX run_chat_owner ON run_chat(task_id,created_at,id);

CREATE TABLE api_keys (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 64),
 token_hash TEXT NOT NULL UNIQUE,
 prefix TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 last_used_at INTEGER NOT NULL DEFAULT 0,
 revoked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE mcp_requests (
 key_id TEXT NOT NULL REFERENCES api_keys(id),
 idempotency_key TEXT NOT NULL,
 tool TEXT NOT NULL,
 args_hash TEXT NOT NULL,
 result TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(key_id, idempotency_key)
);
