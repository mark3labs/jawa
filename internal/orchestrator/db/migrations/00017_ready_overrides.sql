-- +goose Up
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

-- +goose Down
DROP TABLE workflow_ready_overrides;
