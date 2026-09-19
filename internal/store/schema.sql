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

-- Pipeline tables: stage → findings → issues.
--
-- Three tables that evaluate data at three levels. The pipeline between them is
-- fully OPTIONAL: a FloMorphic flow (the expert user's rules) decides where a row
-- lands. Raw data usually arrives on `stage`; a later process may turn a staged
-- row into a `finding` when it shows some aspect worth tracking; a finding that
-- needs validation / a fix / a mission becomes an `issue`. But a flow may just as
-- well write straight to findings or issues — nothing forces the order. The
-- tables share one vocabulary so every row is self-describing:
--
--   source   where the underlying DATA came from (connector / node / feed)
--   origin   which PROCESS produced this row (flow, query, api, manual …)
--   ref      structured provenance: how the row was made (flow id, run, rule,
--            query, upstream ids …) — any shape, kept as-is
--   data     the payload / evidence itself — any shape
--   meta     enrichment / context attached by later processes — any shape
--   *_id     typed links between the tables (0 = not linked)
--
-- `data`, `meta` and `ref` are free-form JSONB precisely because each producer
-- has its own model; the front renders them as an explorable tree.

-- Stage: the pipeline inbox. Raw, un-triaged rows. A flow routes each row via
-- `disposition` (pending|promoted|held|dropped); a promoted row records what it
-- became in `finding_id` / `issue_id`.
CREATE TABLE IF NOT EXISTS stage (
    id          BIGSERIAL   PRIMARY KEY,
    title       TEXT        NOT NULL DEFAULT '',
    summary     TEXT        NOT NULL DEFAULT '',
    source      TEXT        NOT NULL DEFAULT '',
    origin      TEXT        NOT NULL DEFAULT '',
    disposition TEXT        NOT NULL DEFAULT 'pending',
    finding_id  BIGINT      NOT NULL DEFAULT 0,
    issue_id    BIGINT      NOT NULL DEFAULT 0,
    tags        TEXT[]      NOT NULL DEFAULT '{}',
    ref         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    meta        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Columns added after the first release (the CREATE above is a no-op on an
-- existing install, so each addition is repeated here idempotently).
ALTER TABLE stage ADD COLUMN IF NOT EXISTS origin     TEXT   NOT NULL DEFAULT '';
ALTER TABLE stage ADD COLUMN IF NOT EXISTS finding_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE stage ADD COLUMN IF NOT EXISTS ref        JSONB  NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE stage ADD COLUMN IF NOT EXISTS meta       JSONB  NOT NULL DEFAULT '{}'::jsonb;

-- Findings: something a process concluded from data — an observation with a
-- severity, a confidence and a target, still to be validated. Every finding
-- carries its provenance (`origin` + `ref`), the evidence (`data`) and any
-- enrichment (`meta`), plus typed links back to the stage row it came from and
-- forward to the issue it became. `fingerprint` is a producer-chosen dedup key
-- (e.g. rule + target) so a flow can recognise a repeat.
CREATE TABLE IF NOT EXISTS findings (
    id          BIGSERIAL   PRIMARY KEY,
    title       TEXT        NOT NULL,
    summary     TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'new',
    severity    TEXT        NOT NULL DEFAULT 'info',
    confidence  TEXT        NOT NULL DEFAULT '',
    category    TEXT        NOT NULL DEFAULT '',
    tags        TEXT[]      NOT NULL DEFAULT '{}',
    source      TEXT        NOT NULL DEFAULT '',
    origin      TEXT        NOT NULL DEFAULT '',
    target      TEXT        NOT NULL DEFAULT '',
    fingerprint TEXT        NOT NULL DEFAULT '',
    stage_id    BIGINT      NOT NULL DEFAULT 0,
    issue_id    BIGINT      NOT NULL DEFAULT 0,
    ref         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    meta        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Issues: Venapce's single axis/main table — the rows that need validating,
-- fixing, a mission, or otherwise acting on. Rather than a table per issue
-- type, every row carries `tags`, and a saved sub-view is just a tag filter.
-- Rows are produced and advanced by FloMorphic workflows (the auxiliary logic
-- system), directly or by promoting a finding / staged row.
CREATE TABLE IF NOT EXISTS issues (
    id         BIGSERIAL   PRIMARY KEY,
    title      TEXT        NOT NULL,
    summary    TEXT        NOT NULL DEFAULT '',
    status     TEXT        NOT NULL DEFAULT 'open',
    severity   TEXT        NOT NULL DEFAULT 'info',
    tags       TEXT[]      NOT NULL DEFAULT '{}',
    source     TEXT        NOT NULL DEFAULT '',
    origin     TEXT        NOT NULL DEFAULT '',
    assignee   TEXT        NOT NULL DEFAULT '',
    finding_id BIGINT      NOT NULL DEFAULT 0,
    stage_id   BIGINT      NOT NULL DEFAULT 0,
    ref        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    data       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    meta       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE issues ADD COLUMN IF NOT EXISTS origin     TEXT   NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN IF NOT EXISTS finding_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN IF NOT EXISTS stage_id   BIGINT NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN IF NOT EXISTS ref        JSONB  NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE issues ADD COLUMN IF NOT EXISTS meta       JSONB  NOT NULL DEFAULT '{}'::jsonb;

-- Filter by tag overlap (the saved-view mechanism), by lifecycle, and list newest first.
CREATE INDEX IF NOT EXISTS idx_issues_tags ON issues USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_issues_created_at ON issues (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_stage_disposition ON stage (disposition);
CREATE INDEX IF NOT EXISTS idx_findings_tags ON findings USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings (status);
CREATE INDEX IF NOT EXISTS idx_findings_fingerprint ON findings (fingerprint);
CREATE INDEX IF NOT EXISTS idx_findings_created_at ON findings (created_at DESC);
