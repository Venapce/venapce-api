-- name: ListFindings :many
-- Tag filter: match_all=false matches ANY of the tags (&&), true requires ALL (@>).
SELECT * FROM findings
WHERE (cardinality(@tags::text[]) = 0
       OR (@match_all::bool AND tags @> @tags::text[])
       OR (NOT @match_all::bool AND tags && @tags::text[]))
  AND (@status::text = '' OR status = @status::text)
  AND (@severity::text = '' OR severity = @severity::text)
  AND (@category::text = '' OR category = @category::text)
  AND (@source::text = '' OR source = @source::text)
  AND (@target::text = '' OR target = @target::text)
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR summary ILIKE '%' || @search::text || '%'
       OR source ILIKE '%' || @search::text || '%'
       OR origin ILIKE '%' || @search::text || '%'
       OR target ILIKE '%' || @search::text || '%'
       OR fingerprint ILIKE '%' || @search::text || '%')
ORDER BY created_at DESC;

-- name: FindingTags :many
SELECT DISTINCT unnest(tags)::text AS tag FROM findings ORDER BY tag;

-- name: GetFinding :one
SELECT * FROM findings WHERE id = $1;

-- name: ListFindingsByStage :many
SELECT * FROM findings WHERE stage_id = $1 ORDER BY created_at DESC;

-- name: ListFindingsByIssue :many
SELECT * FROM findings WHERE issue_id = $1 ORDER BY created_at DESC;

-- name: CreateFinding :one
INSERT INTO findings (title, summary, status, severity, confidence, category, tags, source, origin,
                      target, fingerprint, stage_id, issue_id, ref, data, meta)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: UpdateFinding :one
UPDATE findings
   SET title = $2, summary = $3, status = $4, severity = $5, confidence = $6, category = $7,
       tags = $8, source = $9, origin = $10, target = $11, fingerprint = $12,
       stage_id = $13, issue_id = $14, ref = $15, data = $16, meta = $17,
       updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteFinding :exec
DELETE FROM findings WHERE id = $1;

-- name: PromoteFinding :one
-- Record the issue a finding became.
UPDATE findings SET status = 'promoted', issue_id = $2, updated_at = now()
 WHERE id = $1
RETURNING *;
