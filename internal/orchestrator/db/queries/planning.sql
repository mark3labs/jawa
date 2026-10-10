-- name: GetPlanningSession :one
SELECT * FROM planning_sessions WHERE card_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1;
-- name: GetPlanningSessionByID :one
SELECT * FROM planning_sessions WHERE id=?;
-- name: ListPlanningMessages :many
SELECT * FROM planning_messages WHERE session_id=? ORDER BY sequence;
-- name: ListPlanningRevisions :many
SELECT * FROM planning_revisions WHERE session_id=? ORDER BY number;
-- name: ListPlanningTurns :many
SELECT * FROM planning_turns WHERE session_id=? ORDER BY number;
-- name: GetPlanningTurn :one
SELECT * FROM planning_turns WHERE id=?;
-- name: ListQueuedPlanningTurns :many
SELECT t.* FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id JOIN cards c ON c.id=s.card_id
WHERE t.state='queued' AND t.published=0 AND s.state='queued' AND c.status='Planning' ORDER BY t.created_at,t.id;
-- name: RetainPlanningDelivery :exec
INSERT OR IGNORE INTO planning_deliveries(turn_id,kind,payload,received_at) VALUES(?,?,?,?);
-- name: ListPlanningCancellations :many
SELECT * FROM planning_turns WHERE state='cancelled' AND published=1 AND cancel_sent=0 AND agent_id<>'' AND run_id<>'' AND remote_attempt_id<>'' ORDER BY updated_at;
-- name: MarkPlanningCancellationSent :exec
UPDATE planning_turns SET cancel_sent=1 WHERE id=? AND state='cancelled' AND agent_id=? AND run_id=? AND remote_attempt_id=?;
