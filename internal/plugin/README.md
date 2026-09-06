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
| `db.issues.upsert` | Insert an issue, or update it in place when an `id` is supplied and already exists. |
| `db.issues.update` | Update an existing issue by `id` (only the filled fields change). |
| `db.stages.upsert` | Insert / upsert a `stage` row. |
| `db.stages.update` | Update a `stage` row by `id`. |
| `osquery.query` | Dispatch an osquery SQL to one enrolled node via osctrl and return the rows it reports (run → poll → collect). |

Meta lookups: `osquery.meta.nodes`, `osquery.meta.environments` back the Node and
Environment pickers on the query form.

The db actions' writable columns are **not hardcoded** — they are derived by
reflecting over the sqlc-generated models (`model.Issue`, `model.Stage` in
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
db/           db.issues.* / db.stages.* over venapce's pgxpool
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
