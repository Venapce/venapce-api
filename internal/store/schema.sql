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
-- Stage is tag-filtered too now (saved views), so it wants the same GIN index.
CREATE INDEX IF NOT EXISTS idx_stage_tags ON stage USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_findings_tags ON findings USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings (status);
CREATE INDEX IF NOT EXISTS idx_findings_fingerprint ON findings (fingerprint);
CREATE INDEX IF NOT EXISTS idx_findings_created_at ON findings (created_at DESC);

-- Activities: the history of a pipeline row. Every row of stage / findings /
-- issues is changed by hand and — above all — by FloMorphic flows, and each of
-- those changes is recorded here against its subject (`subject_kind` +
-- `subject_id`), so a row's timeline is readable and every flow outcome keeps
-- its meaning.
--
-- A `run` activity is one flow executed on one row: the row goes to FloMorphic
-- as the run's context document, the process id / pid / context id are kept
-- here, and once the run ends its OUTCOME is lifted into the typed columns:
--
--   title / description   what the flow concluded, in words
--   remediation           what to do about it
--   proof                 the evidence the conclusion rests on
--   facts                 [{"k":"severity","v":"low"}, …] — typed key/values
--                         the front can identify and render (severity, cve,
--                         package, version, score …)
--   tags                  labels the flow attached
--   data                  the run's full output document, any shape
--
-- `edit` / `promote` / `create` activities record manual and pipeline changes
-- (`ref` holds the field diff). A later flow can read a row's earlier
-- activities (they travel in the context as `history`), so outcomes compound:
-- collect → assess → decide → act.
CREATE TABLE IF NOT EXISTS activities (
    id           BIGSERIAL   PRIMARY KEY,
    subject_kind TEXT        NOT NULL DEFAULT '',
    subject_id   BIGINT      NOT NULL DEFAULT 0,
    kind         TEXT        NOT NULL DEFAULT 'run',
    status       TEXT        NOT NULL DEFAULT 'finished',
    title        TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    remediation  TEXT        NOT NULL DEFAULT '',
    proof        TEXT        NOT NULL DEFAULT '',
    facts        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    tags         TEXT[]      NOT NULL DEFAULT '{}',
    origin       TEXT        NOT NULL DEFAULT '',
    flow_id      TEXT        NOT NULL DEFAULT '',
    flow_title   TEXT        NOT NULL DEFAULT '',
    process_id   BIGINT      NOT NULL DEFAULT 0,
    pid          TEXT        NOT NULL DEFAULT '',
    context_id   TEXT        NOT NULL DEFAULT '',
    error        TEXT        NOT NULL DEFAULT '',
    ref          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    data         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    meta         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_activities_subject ON activities (subject_kind, subject_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_activities_status ON activities (status);
CREATE INDEX IF NOT EXISTS idx_activities_process ON activities (process_id);
CREATE INDEX IF NOT EXISTS idx_activities_tags ON activities USING gin (tags);
CREATE INDEX IF NOT EXISTS idx_activities_created_at ON activities (created_at DESC);

-- ---- Operations: installed packages of flows that originate pipeline data ----
--
-- An operation is the "feature" unit of Venapce: a folder holding one or more
-- FloMorphic workflow exports plus an `operation.json` manifest (schema in the
-- wapp: public/schemas/venapce-operation.schema.json) that says what the estate
-- must provide (osctrl, plugins, settings profiles, other operations) and which
-- `params` adapt it to one organization. The whole package is kept here — the
-- manifest and every file it names, as text — so a flow can be (re)installed
-- into FloMorphic at any time with the operator's params substituted, and the
-- package can be re-read from its source for an upgrade.
--
--   key        manifest.id — one install per key
--   manifest   the parsed operation.json
--   files      {path: text} — the flow exports, README, per-flow docs
--   source     where it came from ({kind:url|folder|paste, url, path, ref, folder})
--   params     operator values of the manifest params (secret ones excluded)
--   secrets    {name: ciphertext} — secret params, AES-GCM, never returned
--   bindings   {flowKey: {flowId, flowTitle, installedAt}} — which FloMorphic
--              flow each manifest flow became
--
-- Runs of an operation's entry flows are activities with subject_kind
-- 'operation' and subject_id = operations.id.
CREATE TABLE IF NOT EXISTS operations (
    id           BIGSERIAL   PRIMARY KEY,
    key          TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL DEFAULT '',
    version      TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    tags         TEXT[]      NOT NULL DEFAULT '{}',
    scale        TEXT[]      NOT NULL DEFAULT '{}',
    manifest     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    files        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    source       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    params       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    secrets      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    bindings     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_operations_tags ON operations USING gin (tags);

-- ---- Saved views: a named tag filter over one of the pipeline tables ----
--
-- How an operator "adds a submenu" without a new table: name a set of tags and
-- which table they filter (stage | findings | issues | activities), and the
-- sidebar gains an entry that opens that table carved down to those rows.
-- `match_mode` says whether a row needs ANY of the tags ('any', overlap) or ALL
-- of them ('all', contains) — the same semantics the list endpoints take.
-- Stored server-side so a view follows the operator across browsers.
CREATE TABLE IF NOT EXISTS views (
    id          BIGSERIAL   PRIMARY KEY,
    name        TEXT        NOT NULL,
    target      TEXT        NOT NULL DEFAULT 'issues',
    tags        TEXT[]      NOT NULL DEFAULT '{}',
    match_mode  TEXT        NOT NULL DEFAULT 'any',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_views_target ON views (target);
