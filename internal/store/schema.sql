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

-- Stage: the pipeline inbox that precedes issues. Everything a pipeline feeds into
-- FloMorphic lands here first (raw, un-triaged). A FloMorphic flow then routes each
-- row via `disposition` (pending|promoted|held|dropped); a promoted row records the
-- issue it became in `issue_id` (0 = not promoted). `data` is the raw payload.
CREATE TABLE IF NOT EXISTS stage (
    id          BIGSERIAL   PRIMARY KEY,
    title       TEXT        NOT NULL DEFAULT '',
    summary     TEXT        NOT NULL DEFAULT '',
    source      TEXT        NOT NULL DEFAULT '',
    disposition TEXT        NOT NULL DEFAULT 'pending',
    issue_id    BIGINT      NOT NULL DEFAULT 0,
    tags        TEXT[]      NOT NULL DEFAULT '{}',
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Issues: Venapce's single axis/main table. Rather than a table per issue type,
-- every row carries `tags`, and a saved sub-view is just a tag filter. Rows are
-- produced and advanced by FloMorphic workflows (the auxiliary logic system).
CREATE TABLE IF NOT EXISTS issues (
    id         BIGSERIAL   PRIMARY KEY,
    title      TEXT        NOT NULL,
    summary    TEXT        NOT NULL DEFAULT '',
    status     TEXT        NOT NULL DEFAULT 'open',
    severity   TEXT        NOT NULL DEFAULT 'info',
    tags       TEXT[]      NOT NULL DEFAULT '{}',
    source     TEXT        NOT NULL DEFAULT '',
    assignee   TEXT        NOT NULL DEFAULT '',
    data       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Filter issues by tag overlap (the saved-view mechanism) and list newest first.
CREATE INDEX IF NOT EXISTS idx_issues_tags ON issues USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_issues_created_at ON issues (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_stage_disposition ON stage (disposition);
