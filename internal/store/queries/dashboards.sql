-- name: ListDashboards :many
SELECT * FROM dashboards ORDER BY updated_at DESC;

-- name: GetDashboard :one
SELECT * FROM dashboards WHERE id = $1;

-- name: CreateDashboard :one
INSERT INTO dashboards (title, slug, layout)
VALUES ($1, $2, $3)
RETURNING *;

-- name: UpdateDashboard :one
UPDATE dashboards
   SET title = $2, slug = $3, layout = $4, updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteDashboard :exec
DELETE FROM dashboards WHERE id = $1;
