-- +goose Up
ALTER TABLE cards ADD COLUMN auto_merge_mode TEXT NOT NULL DEFAULT 'inherit' CHECK(auto_merge_mode IN ('inherit','on','off'));
ALTER TABLE issue_submissions ADD COLUMN auto_merge_mode TEXT NOT NULL DEFAULT 'inherit' CHECK(auto_merge_mode IN ('inherit','on','off'));
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
-- +goose StatementBegin
CREATE TRIGGER auto_merge_card_update BEFORE UPDATE OF status,archived,project_id ON cards
WHEN EXISTS(SELECT 1 FROM auto_merge_operations WHERE card_id=OLD.id AND state IN ('reserved','inflight'))
BEGIN SELECT RAISE(ABORT,'external merge reserved; wait for reconciliation'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER auto_merge_card_delete BEFORE DELETE ON cards
WHEN EXISTS(SELECT 1 FROM auto_merge_operations WHERE card_id=OLD.id AND state IN ('reserved','inflight'))
BEGIN SELECT RAISE(ABORT,'external merge reserved; wait for reconciliation'); END;
-- +goose StatementEnd
