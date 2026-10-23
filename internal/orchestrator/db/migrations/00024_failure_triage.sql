-- +goose Up
-- Decision-model triage of the current failure generation. Empty means not
-- yet checked; a new failure_token resets it. Only the class and its
-- probability are kept, never the error text sent to the model.
ALTER TABLE auto_resumes ADD COLUMN triage TEXT NOT NULL DEFAULT '' CHECK(triage IN ('','transient','permanent','unavailable'));
ALTER TABLE auto_resumes ADD COLUMN triage_probability REAL NOT NULL DEFAULT 0;
