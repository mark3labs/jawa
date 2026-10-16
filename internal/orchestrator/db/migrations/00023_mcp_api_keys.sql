-- +goose Up
CREATE TABLE api_keys (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 64),
 token_hash TEXT NOT NULL UNIQUE,
 prefix TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 last_used_at INTEGER NOT NULL DEFAULT 0,
 revoked_at INTEGER NOT NULL DEFAULT 0
);
-- Durable replay records for MCP write tools that are not naturally idempotent.
CREATE TABLE mcp_requests (
 key_id TEXT NOT NULL REFERENCES api_keys(id),
 idempotency_key TEXT NOT NULL,
 tool TEXT NOT NULL,
 args_hash TEXT NOT NULL,
 result TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(key_id, idempotency_key)
);
-- +goose Down
DROP TABLE mcp_requests;
DROP TABLE api_keys;
