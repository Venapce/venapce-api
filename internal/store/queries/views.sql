-- name: ListViews :many
-- Every saved view, oldest first so the sidebar order is stable as views are added.
SELECT * FROM views
WHERE (@target::text = '' OR target = @target::text)
ORDER BY created_at;

-- name: GetView :one
SELECT * FROM views WHERE id = $1;

-- name: CreateView :one
INSERT INTO views (name, target, tags, match_mode)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateView :one
-- Full-row update of the editable columns; the handler merges a partial body
-- onto the current row first, so this always writes every field.
UPDATE views
   SET name = $2, target = $3, tags = $4, match_mode = $5, updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteView :exec
DELETE FROM views WHERE id = $1;
