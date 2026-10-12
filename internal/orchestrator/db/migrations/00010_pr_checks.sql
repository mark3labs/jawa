-- +goose Up
CREATE TABLE workflow_pr_checks (
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

-- +goose Down
DROP TABLE workflow_pr_checks;
