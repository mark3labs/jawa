-- +goose Up
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
-- +goose Down
SELECT 1;
