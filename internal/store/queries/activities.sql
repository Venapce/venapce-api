-- name: ListActivities :many
-- Tag filter: match_all=false matches ANY of the tags (&&), true requires ALL (@>).
SELECT * FROM activities
WHERE (@subject_kind::text = '' OR subject_kind = @subject_kind::text)
  AND (@subject_id::bigint = 0 OR subject_id = @subject_id::bigint)
  AND (@kind::text = '' OR kind = @kind::text)
  AND (@status::text = '' OR status = @status::text)
  AND (@flow_id::text = '' OR flow_id = @flow_id::text)
  AND (cardinality(@tags::text[]) = 0
       OR (@match_all::bool AND tags @> @tags::text[])
       OR (NOT @match_all::bool AND tags && @tags::text[]))
  AND (@search::text = ''
       OR title ILIKE '%' || @search::text || '%'
       OR description ILIKE '%' || @search::text || '%'
       OR remediation ILIKE '%' || @search::text || '%'
       OR flow_title ILIKE '%' || @search::text || '%'
       OR origin ILIKE '%' || @search::text || '%')
ORDER BY created_at DESC
LIMIT @lim::int;

-- name: ListActivitiesBySubject :many
SELECT * FROM activities
WHERE subject_kind = $1 AND subject_id = $2
ORDER BY created_at DESC;

-- name: ListOpenActivities :many
-- Runs still in flight on FloMorphic — what the reconciler polls.
SELECT * FROM activities
WHERE process_id <> 0 AND status IN ('scheduled', 'running', 'waiting')
ORDER BY created_at ASC
LIMIT $1;

-- name: ActivityTags :many
SELECT DISTINCT unnest(tags)::text AS tag FROM activities ORDER BY tag;

-- name: GetActivity :one
SELECT * FROM activities WHERE id = $1;

-- name: CreateActivity :one
INSERT INTO activities (subject_kind, subject_id, kind, status, title, description, remediation, proof,
                        facts, tags, origin, flow_id, flow_title, process_id, pid, context_id, error,
                        ref, data, meta, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
RETURNING *;

-- name: UpdateActivity :one
UPDATE activities
   SET subject_kind = $2, subject_id = $3, kind = $4, status = $5, title = $6, description = $7,
       remediation = $8, proof = $9, facts = $10, tags = $11, origin = $12, flow_id = $13,
       flow_title = $14, process_id = $15, pid = $16, context_id = $17, error = $18,
       ref = $19, data = $20, meta = $21, started_at = $22, finished_at = $23,
       updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteActivity :exec
DELETE FROM activities WHERE id = $1;

-- name: DeleteActivitiesBySubject :exec
DELETE FROM activities WHERE subject_kind = $1 AND subject_id = $2;
