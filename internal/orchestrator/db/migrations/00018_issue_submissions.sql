-- +goose Up
CREATE TABLE issue_submissions (
 token TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 provider TEXT NOT NULL,
 repository TEXT NOT NULL,
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('reserved','rejected','uncertain','created','completed')),
 issue_url TEXT NOT NULL DEFAULT '',
 issue_number INTEGER NOT NULL DEFAULT 0,
 card_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
-- Deliberately no cascading foreign keys: deletion must not erase replay evidence.
-- +goose Down
SELECT 1;
