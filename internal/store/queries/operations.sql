-- name: ListOperations :many
SELECT * FROM operations ORDER BY name ASC, id ASC;

-- name: GetOperation :one
SELECT * FROM operations WHERE id = $1;

-- name: GetOperationByKey :one
SELECT * FROM operations WHERE key = $1;

-- name: CreateOperation :one
INSERT INTO operations (key, name, version, description, tags, scale, manifest, files, source, params, secrets, bindings)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: UpdateOperation :one
UPDATE operations
   SET name = $2, version = $3, description = $4, tags = $5, scale = $6, manifest = $7, files = $8,
       source = $9, params = $10, secrets = $11, bindings = $12, updated_at = now()
 WHERE id = $1
RETURNING *;

-- name: DeleteOperation :exec
DELETE FROM operations WHERE id = $1;
