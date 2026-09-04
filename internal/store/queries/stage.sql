-- name: ListStage :many
SELECT id, title, summary, source, disposition, issue_id, tags, data, received_at, updated_at
FROM stage
WHERE (@disposition::text = '' OR disposition = @disposition::text)
  AND (@source::text = '' OR source = @source::text)
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR summary ILIKE '%' || @search::text || '%'
       OR source ILIKE '%' || @search::text || '%')
ORDER BY received_at DESC;

-- name: GetStage :one
SELECT id, title, summary, source, disposition, issue_id, tags, data, received_at, updated_at
FROM stage WHERE id = $1;

-- name: CreateStage :one
INSERT INTO stage (title, summary, source, disposition, tags, data)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, title, summary, source, disposition, issue_id, tags, data, received_at, updated_at;

-- name: PromoteStage :one
-- Mark a staged row as promoted and record the issue it became.
UPDATE stage SET disposition = 'promoted', issue_id = $2, updated_at = now()
WHERE id = $1
RETURNING id, title, summary, source, disposition, issue_id, tags, data, received_at, updated_at;
