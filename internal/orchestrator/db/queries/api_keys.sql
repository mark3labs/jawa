-- name: InsertAPIKey :exec
INSERT INTO api_keys(id,name,token_hash,prefix,created_at) VALUES(?,?,?,?,?);

-- name: ListAPIKeys :many
SELECT id,name,prefix,created_at,last_used_at,revoked_at FROM api_keys
ORDER BY revoked_at<>0, created_at DESC, id;

-- name: GetActiveAPIKeyByHash :one
SELECT id,name FROM api_keys WHERE token_hash = ? AND revoked_at = 0;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND last_used_at < sqlc.arg(before);

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND revoked_at = 0;
