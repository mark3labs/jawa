-- +goose Up
ALTER TABLE cards ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1));

-- +goose Down
ALTER TABLE cards DROP COLUMN archived;
