-- +goose Up
CREATE TABLE workflow_pr_merge_overrides (
 attempt_id TEXT PRIMARY KEY REFERENCES workflow_attempts(id) ON DELETE CASCADE,
 pr_url TEXT NOT NULL, provider TEXT NOT NULL, repo TEXT NOT NULL,
 base_branch TEXT NOT NULL, marked_at INTEGER NOT NULL);
-- +goose Down
DROP TABLE workflow_pr_merge_overrides;
