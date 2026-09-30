-- name: ListStage :many
-- Filter by tags (empty = no tag filter). match_all=false matches ANY of the
-- tags (overlap, &&); match_all=true requires ALL of them (contains, @>) — the
-- same semantics as ListIssues / ListFindings.
SELECT * FROM stage
WHERE (cardinality(@tags::text[]) = 0
       OR (@match_all::bool AND tags @> @tags::text[])
       OR (NOT @match_all::bool AND tags && @tags::text[]))
  AND (@disposition::text = '' OR disposition = @disposition::text)
  AND (@source::text = '' OR source = @source::text)
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR summary ILIKE '%' || @search::text || '%'
       OR source ILIKE '%' || @search::text || '%'
       OR origin ILIKE '%' || @search::text || '%')
ORDER BY received_at DESC;

-- name: StageTags :many
-- Distinct tags across the inbox, for the tag picker when defining a view.
SELECT DISTINCT unnest(tags)::text AS tag FROM stage ORDER BY tag;

-- name: GetStage :one
SELECT * FROM stage WHERE id = $1;

-- name: CreateStage :one
INSERT INTO stage (title, summary, source, origin, disposition, tags, ref, data, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateStage :one
-- Full-row update of the editable columns; the handler merges a partial body
-- onto the current row first, so this always writes every field.
UPDATE stage
   SET title = $2, summary = $3, source = $4, origin = $5, disposition = $6,
       finding_id = $7, issue_id = $8, tags = $9, ref = $10, data = $11, meta = $12,
       updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteStage :exec
DELETE FROM stage WHERE id = $1;

-- name: PromoteStage :one
-- Mark a staged row as promoted and record what it became: a finding, an
-- issue, or both (0 leaves a link untouched).
UPDATE stage
   SET disposition = 'promoted',
       finding_id = CASE WHEN @finding_id::bigint = 0 THEN finding_id ELSE @finding_id::bigint END,
       issue_id   = CASE WHEN @issue_id::bigint   = 0 THEN issue_id   ELSE @issue_id::bigint   END,
       updated_at = now()
 WHERE id = @id
RETURNING *;
