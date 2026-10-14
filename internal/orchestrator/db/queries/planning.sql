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
-- name: ListPlanningInteractions :many
SELECT i.*,r.kind,r.tool_call_id FROM planning_interactions i
JOIN typed_reports r ON r.report_id=i.report_id
JOIN planning_turns t ON t.id=i.task_id WHERE t.session_id=? ORDER BY i.created_at,i.rowid;
-- name: ListQueuedPlanningReplies :many
SELECT report_id FROM planning_interactions WHERE status='queued' AND waiting_json<>'' ORDER BY created_at;
-- name: ListPlanningRunHistory :many
SELECT t.id,t.session_id,t.number,t.state,t.agent_id,t.run_id,t.remote_attempt_id,
 t.result,t.error,t.created_at,t.updated_at,s.card_id,s.state AS session_state,
 EXISTS(SELECT 1 FROM planning_turns newer WHERE newer.session_id=t.session_id AND newer.number>t.number) AS superseded,
 COALESCE(i.status,'') AS interaction_status,COALESCE(r.kind,'') AS interaction_kind,
 EXISTS(SELECT 1 FROM failed_resumes f WHERE f.task_id=t.id AND f.status='pending') AS pending_resume
FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id
LEFT JOIN planning_interactions i ON i.report_id=(SELECT x.report_id FROM planning_interactions x WHERE x.task_id=t.id ORDER BY x.created_at DESC,x.rowid DESC LIMIT 1)
LEFT JOIN typed_reports r ON r.report_id=i.report_id;
-- name: GetPlanningRunHistory :one
SELECT t.id,t.session_id,t.number,t.state,t.agent_id,t.run_id,t.remote_attempt_id,
 t.result,t.error,t.created_at,t.updated_at,s.card_id,s.state AS session_state,
 EXISTS(SELECT 1 FROM planning_turns newer WHERE newer.session_id=t.session_id AND newer.number>t.number) AS superseded,
 COALESCE(i.status,'') AS interaction_status,COALESCE(r.kind,'') AS interaction_kind,
 EXISTS(SELECT 1 FROM failed_resumes f WHERE f.task_id=t.id AND f.status='pending') AS pending_resume
FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id
LEFT JOIN planning_interactions i ON i.report_id=(SELECT x.report_id FROM planning_interactions x WHERE x.task_id=t.id ORDER BY x.created_at DESC,x.rowid DESC LIMIT 1)
LEFT JOIN typed_reports r ON r.report_id=i.report_id WHERE t.id=?;
-- name: ListPlanningRunDeliveries :many
SELECT kind,payload,received_at FROM planning_deliveries WHERE turn_id=? ORDER BY received_at DESC,id DESC LIMIT 256;
