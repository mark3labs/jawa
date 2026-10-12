-- name: GetCard :one
SELECT * FROM cards WHERE id = ?;

-- name: ListCards :many
SELECT * FROM cards
WHERE (sqlc.arg(project_id) = '' OR project_id = sqlc.arg(project_id))
ORDER BY CASE status WHEN 'Todo' THEN 0 WHEN 'Planning' THEN 1 WHEN 'Building' THEN 2 ELSE 3 END,
position, created_at, id;

-- name: InsertCard :exec
INSERT INTO cards(id,project_id,title,description,status,position,created_at,issue_url)
VALUES(?,?,?,?,?,?,?,?);

-- name: UpdateCardLane :execrows
UPDATE cards SET status = ?, position = ? WHERE id = ? AND project_id = ?;

-- name: ListCardLane :many
SELECT id FROM cards WHERE project_id = ? AND status = ? AND id <> sqlc.arg(exclude_id)
ORDER BY position,created_at,id;

-- name: NextCardPosition :one
SELECT CAST(coalesce(max(position)+1,0) AS INTEGER) AS position FROM cards WHERE project_id = ? AND status = ?;

-- name: SetCardArchived :execrows
UPDATE cards SET archived = ? WHERE id = ?;

-- name: GetProject :one
SELECT * FROM projects WHERE id = ?;
