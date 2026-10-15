-- +goose Up
CREATE TABLE run_chat (
 id TEXT PRIMARY KEY,
 card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 mode TEXT NOT NULL CHECK(mode IN ('Planning','Build','Fix')),
 local_id TEXT NOT NULL, client_id TEXT NOT NULL,
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 text TEXT NOT NULL, classification TEXT NOT NULL,
 interaction TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','dispatched','active','completed','failed','ambiguous')),
 failure TEXT NOT NULL DEFAULT '', request_json TEXT NOT NULL DEFAULT '', receipt_json TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(mode,local_id,client_id)
);
CREATE INDEX run_chat_queue ON run_chat(status,created_at,id);
CREATE INDEX run_chat_owner ON run_chat(task_id,created_at,id);
-- +goose Down
DROP TABLE run_chat;
