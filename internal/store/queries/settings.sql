-- name: GetSetting :one
SELECT key, value, updated_at FROM settings WHERE key = $1;

-- name: UpsertSetting :one
INSERT INTO settings (key, value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value, updated_at = now()
RETURNING key, value, updated_at;
