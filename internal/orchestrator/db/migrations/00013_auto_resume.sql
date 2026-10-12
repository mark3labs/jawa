-- +goose Up
ALTER TABLE failed_resumes ADD COLUMN automatic INTEGER NOT NULL DEFAULT 0 CHECK(automatic IN (0,1));
CREATE TABLE auto_resumes (
 task_id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 planning INTEGER NOT NULL CHECK(planning IN (0,1)), failure_token TEXT NOT NULL,
 due_at INTEGER NOT NULL, next_check_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('scheduled','reserved','stopped','exhausted')));
CREATE INDEX auto_resume_due ON auto_resumes(state,next_check_at,task_id);
-- +goose Down
SELECT 1;
