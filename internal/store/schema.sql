-- Venapce backend metadata schema (operational state only — analytics data lives
-- in the stores Superset reads, never here). Applied idempotently on boot and used
-- by sqlc for type generation. Keep every table CREATE ... IF NOT EXISTS.

-- Key/value app settings. The Superset connection lives here under key 'superset'
-- as { "url": ..., "username": ..., "password_enc": <AES-GCM base64> }.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A saved chart: everything needed to re-query Superset (query_context) plus the
-- builder UI state so the front can reopen it for editing. Charts are reusable and
-- referenced from a dashboard's layout by id.
CREATE TABLE IF NOT EXISTS charts (
    id            BIGSERIAL PRIMARY KEY,
    title         TEXT        NOT NULL,
    viz_type      TEXT        NOT NULL DEFAULT 'bar',
    query_context JSONB       NOT NULL DEFAULT '{}'::jsonb,
    builder_state JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A native Venapce dashboard. `layout` is the grid arrangement: an array of cells
-- [{ "chartId": 1, "x": 0, "y": 0, "w": 6, "h": 8 }, ...] that the drag/resize
-- editor reads and writes. Charts are looked up from the charts table by chartId.
CREATE TABLE IF NOT EXISTS dashboards (
    id         BIGSERIAL PRIMARY KEY,
    title      TEXT        NOT NULL,
    slug       TEXT        NOT NULL DEFAULT '',
    layout     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
