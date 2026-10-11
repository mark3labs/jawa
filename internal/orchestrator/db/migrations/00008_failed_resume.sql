-- +goose Up
CREATE TABLE failed_resumes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 planning INTEGER NOT NULL, request_json TEXT NOT NULL, previous_outcome TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', reply_json TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE UNIQUE INDEX failed_resume_pending ON failed_resumes(task_id) WHERE status='pending';
-- +goose Down
DROP TABLE failed_resumes;
