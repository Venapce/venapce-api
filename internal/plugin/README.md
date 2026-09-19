# Venapce plugin (in-process)

The Inflowenger/FloMorphic **plugin node** that ships *inside* `venapce-api`. It is
not a standalone binary: it connects to infra over NATS using the plugin env
venapce already stores (pasted from the FloMorphic panel — see
[`internal/httpapi/flomorphic.go`](../httpapi/flomorphic.go)), and its handlers use
venapce's **own** database pool and osctrl client. There is therefore **no settings
profile** — the connections it needs are already venapce's.

## Actions

| Method | What it does |
| --- | --- |
| `db.stages.upsert` | Insert / upsert a `stage` row (raw, un-triaged data — the pipeline inbox). |
| `db.stages.update` | Update a `stage` row by `id`. |
| `db.findings.upsert` | Insert / upsert a `finding` (a conclusion drawn from data: severity, confidence, target, provenance). |
| `db.findings.update` | Update a `finding` by `id`. |
| `db.issues.upsert` | Insert an issue, or update it in place when an `id` is supplied and already exists. |
| `db.issues.update` | Update an existing issue by `id` (only the filled fields change). |
| `db.activities.upsert` | Record an activity on a subject row (`subject_kind` + `subject_id`): a note, a conclusion, an outcome of the flow's own. |
| `db.activities.update` | Write a run's outcome onto the activity venapce opened for it: `id = {{$.activity.id}}`, then `title`, `description`, `remediation`, `proof`, `facts`, `tags`, `data`. |

The three tables form an **optional** pipeline — stage → findings → issues — and a
flow writes to whichever level its rules decide, in any order. They share one
vocabulary so every row is self-describing: `source` (where the data came from),
`origin` (which process produced the row), `ref` (structured provenance — how it
was made, any shape), `data` (the payload / evidence, any shape), `meta`
(enrichment, any shape), `tags`, and typed `stage_id` / `finding_id` / `issue_id`
links between the levels (0 = not linked).

**Activities** are the history of a row — manual edits, promotions and, above
all, **flow runs**. When an operator sends a row through a flow (venapce
`POST /api/activities/run`), the run's context document is:

```json
{
  "subject":  { "kind": "stage", "id": 21 },
  "row":      { "...the row: title, tags, data, meta, ref..." },
  "activity": { "id": 4, "flowId": "flow_…", "flowTitle": "…" },
  "history":  [ { "id": 3, "kind": "run", "title": "…", "facts": [], "…": "earlier finished activities, newest first" } ],
  "input":    "optional extra input given at launch",
  "outcome":  {}
}
```

The flow reads the row through `{{$.row.…}}` tokens (its author keeps the flow's
expectations and the row's data model in step) and leaves its conclusion in
`$.outcome` — e.g. a `js` node with key `outcome` — using these keys:

| key | meaning |
| --- | --- |
| `title`, `description` | what the flow concluded, in words |
| `remediation` | what to do about it |
| `proof` | the evidence (text, or any JSON — kept compact) |
| `facts` | `[{"k":"severity","v":"low"}, …]` or `{"severity":"low"}` — typed key/values the front identifies and renders |
| `tags` | labels for the activity |
| `data` | the output document to keep (default: the whole final context minus `row`/`history`) |
| `meta` | any enrichment |

When the process ends, venapce reads the context back and lifts `outcome` into
the activity's columns. A flow can also write them directly with
`db.activities.update` (`id = {{$.activity.id}}`) — a key the outcome does not
set never overwrites what the flow already wrote.
| `osquery.query` | Dispatch an osquery SQL to one enrolled node via osctrl and return the rows it reports (run → poll → collect). |

Meta lookups: `osquery.meta.nodes`, `osquery.meta.environments` back the Node and
Environment pickers on the query form.

The db actions' writable columns are **not hardcoded** — they are derived by
reflecting over the sqlc-generated models (`model.Issue`, `model.Finding`, `model.Stage` in
[`internal/db`](../db)), so when the issues/stage schema changes and sqlc
regenerates, this module follows automatically (a new column of a supported type
becomes a writable field; `RETURNING *` carries it back). Column names come only
from the generated model, never user input, so the SQL never interpolates an
untrusted identifier — every value is a bound parameter. String fields accept
`{{$.path}}` flow tokens.

## Layout

```
plugin.go     intro + registers every module's actions/metas (no settings form)
manager.go    lifecycle: connect/restart from the stored env; best-effort drain
flow/         shared {{$.path}} resolver + flat meta-body decoding
db/           db.stages.* / db.findings.* / db.issues.* over venapce's pgxpool
osquery/      osquery.query + node/env pickers over venapce's osctrl.Manager
```

## Lifecycle

`plugin.Manager` is started at boot from the stored env (`main.go` →
`Server.StartVenapcePlugin`) and re-started when the operator saves a new env
(`PUT /api/settings/flomorphic`) or hits
`POST /api/settings/flomorphic/plugin/restart`. `Start` is idempotent: it
fingerprints the env and no-ops when unchanged. Status (running / pluginId /
error) is surfaced on `GET /api/settings/flomorphic`.

> The distributed-query calls live in
> [`internal/osctrl/query.go`](../osctrl/query.go) — `RunQuery`, `QueryStatus`,
> `QueryResults` — layered on the existing osctrl auth client.
