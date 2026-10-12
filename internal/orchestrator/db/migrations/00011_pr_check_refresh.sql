-- +goose Up
CREATE TABLE workflow_pr_checks_new (
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

INSERT INTO workflow_pr_checks_new(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,error,checked_at) SELECT attempt_id,pr_url,provider,repo,base_branch,state,head_sha,error,checked_at FROM workflow_pr_checks;
DROP TABLE workflow_pr_checks;
ALTER TABLE workflow_pr_checks_new RENAME TO workflow_pr_checks;

-- +goose Down
CREATE TABLE workflow_pr_checks_old (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL,
 provider TEXT NOT NULL,
 repo TEXT NOT NULL,
 base_branch TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('conflict','clear','unknown','error','merged','closed')),
 head_sha TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL
);

INSERT INTO workflow_pr_checks_old(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,error,checked_at) SELECT attempt_id,pr_url,provider,repo,base_branch,CASE WHEN state='checking' THEN 'unknown' ELSE state END,head_sha,error,checked_at FROM workflow_pr_checks;
DROP TABLE workflow_pr_checks;
ALTER TABLE workflow_pr_checks_old RENAME TO workflow_pr_checks;
