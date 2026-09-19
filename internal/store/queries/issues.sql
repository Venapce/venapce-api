-- name: ListIssues :many
-- Filter by tags (empty = no tag filter). match_all=false matches ANY of the
-- tags (overlap, &&); match_all=true requires ALL of them (contains, @>).
SELECT * FROM issues
WHERE (cardinality(@tags::text[]) = 0
       OR (@match_all::bool AND tags @> @tags::text[])
       OR (NOT @match_all::bool AND tags && @tags::text[]))
  AND (@status::text = '' OR status = @status::text)
  AND (@severity::text = '' OR severity = @severity::text)
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR summary ILIKE '%' || @search::text || '%'
       OR source ILIKE '%' || @search::text || '%'
       OR origin ILIKE '%' || @search::text || '%'
       OR assignee ILIKE '%' || @search::text || '%')
ORDER BY created_at DESC;

-- name: IssueTags :many
-- Distinct tags across all issues, for the tag picker when defining a view.
SELECT DISTINCT unnest(tags)::text AS tag FROM issues ORDER BY tag;

-- name: GetIssue :one
SELECT * FROM issues WHERE id = $1;

-- name: CreateIssue :one
INSERT INTO issues (title, summary, status, severity, tags, source, origin, assignee,
                    finding_id, stage_id, ref, data, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: UpdateIssue :one
UPDATE issues
   SET title = $2, summary = $3, status = $4, severity = $5, tags = $6, source = $7,
       origin = $8, assignee = $9, finding_id = $10, stage_id = $11,
       ref = $12, data = $13, meta = $14, updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteIssue :exec
DELETE FROM issues WHERE id = $1;
