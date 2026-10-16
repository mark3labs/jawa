-- name: GetMCPRequest :one
SELECT tool,args_hash,result FROM mcp_requests WHERE key_id = ? AND idempotency_key = ?;

-- name: InsertMCPRequest :exec
INSERT INTO mcp_requests(key_id,idempotency_key,tool,args_hash,result,created_at) VALUES(?,?,?,?,?,?);
