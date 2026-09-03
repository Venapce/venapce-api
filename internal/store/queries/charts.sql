-- name: ListCharts :many
SELECT * FROM charts ORDER BY updated_at DESC;

-- name: GetChart :one
SELECT * FROM charts WHERE id = $1;

-- name: CreateChart :one
INSERT INTO charts (title, viz_type, query_context, builder_state)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateChart :one
UPDATE charts
   SET title = $2, viz_type = $3, query_context = $4, builder_state = $5, updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteChart :exec
DELETE FROM charts WHERE id = $1;
