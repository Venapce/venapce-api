-- name: ListIssues :many
-- Filter by tags (empty = no tag filter). match_all=false matches ANY of the
-- tags (overlap, &&); match_all=true requires ALL of them (contains, @>).
SELECT id, title, summary, status, severity, tags, source, assignee, data, created_at, updated_at
FROM issues
WHERE (cardinality(@tags::text[]) = 0
       OR (@match_all::bool AND tags @> @tags::text[])
       OR (NOT @match_all::bool AND tags && @tags::text[]))
  AND (@status::text = '' OR status = @status::text)
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR summary ILIKE '%' || @search::text || '%'
       OR source ILIKE '%' || @search::text || '%')
ORDER BY created_at DESC;

-- name: IssueTags :many
-- Distinct tags across all issues, for the tag picker when defining a view.
SELECT DISTINCT unnest(tags)::text AS tag FROM issues ORDER BY tag;

-- name: GetIssue :one
SELECT id, title, summary, status, severity, tags, source, assignee, data, created_at, updated_at
FROM issues WHERE id = $1;

-- name: CreateIssue :one
INSERT INTO issues (title, summary, status, severity, tags, source, assignee, data)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, title, summary, status, severity, tags, source, assignee, data, created_at, updated_at;
