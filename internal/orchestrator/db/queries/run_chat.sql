-- name: ListRunChatHistory :many
SELECT * FROM run_chat WHERE mode=? AND local_id=? ORDER BY created_at,rowid;
-- name: ListPendingRunChat :many
SELECT * FROM run_chat m WHERE status IN ('queued','dispatched','active')
AND NOT EXISTS(SELECT 1 FROM run_chat p WHERE p.task_id=m.task_id
AND (p.created_at<m.created_at OR (p.created_at=m.created_at AND p.rowid<m.rowid))
AND p.status IN ('queued','dispatched','active','ambiguous'))
ORDER BY created_at,rowid LIMIT 16;
