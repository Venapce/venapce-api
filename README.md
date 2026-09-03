# venapce-api

The Venapce backend — a **Go + Fiber** service that fronts Superset for the Vue
dashboard builder and stores native Venapce dashboards in **Postgres**.

It exists so the browser stops talking to Superset directly: the service account
lives here, this service logs in, refreshes the token, injects CSRF, and proxies
the data endpoints (integration study §4). The old "Connect to Superset" login
screen goes away — Superset is now just a **connection configured in Settings**.

## Stack

Go 1.26 · [Fiber v2](https://gofiber.io) · Postgres (pgx/v5) · [sqlc](https://sqlc.dev) · AES-GCM for secrets at rest.

## Layout

```
cmd/api/main.go              entrypoint: connect PG, migrate, load settings, listen
internal/config              env-driven config
internal/cryptobox           AES-256-GCM encrypt/decrypt (Superset password at rest)
internal/superset            server-side Superset REST client + live-client manager
internal/store               schema.sql (source of truth) + sqlc queries + boot migrate
internal/db                  sqlc-GENERATED typed queries (do not edit)
internal/httpapi             Fiber server + handlers (settings, superset proxy, charts, dashboards)
```

`internal/db` is generated — edit `internal/store/schema.sql` or
`internal/store/queries/*.sql` and run `make generate`.

## API

| Method & path | Purpose |
|---|---|
| `GET /healthz` | liveness |
| `GET /api/settings/superset` | connection status (no secrets) |
| `PUT /api/settings/superset` | save URL + username + password (encrypted), probe login |
| `POST /api/settings/superset/test` | re-probe the stored connection |
| `GET /api/superset/databases` | proxy: registered databases |
| `GET /api/superset/datasets?search=` | proxy: datasets |
| `GET /api/superset/datasets/:id` | proxy: one dataset (columns + metrics) |
| `GET /api/superset/dashboards` | proxy: Superset's own dashboards (metadata) |
| `POST /api/superset/chart/data` | proxy: send a `query_context`, get computed rows |
| `GET/POST /api/charts`, `GET/PUT/DELETE /api/charts/:id` | native saved charts |
| `GET/POST /api/dashboards`, `GET/PUT/DELETE /api/dashboards/:id` | native dashboards (title + grid `layout`) |

### The native dashboard model

A `dashboard.layout` is the grid arrangement the drag/resize editor reads and
writes — an array of cells referencing saved charts by id:

```json
[{ "chartId": 1, "x": 0, "y": 0, "w": 6, "h": 8 }]
```

Each `chart` carries its `query_context` (what to ask Superset) and `builder_state`
(so the front can reopen it for editing).

## Run locally

```bash
cp .env.example .env          # point DATABASE_URL at a reachable Postgres
make generate                 # only after changing schema/queries
make run                      # http://localhost:8080
```

The schema is applied on boot (idempotent `CREATE TABLE IF NOT EXISTS`).

## Deploy

Fits the shared-Postgres `deploy/docker-compose.yml`: the `venapce` database and
role are already provisioned by `deploy/postgres-init`, and the `venapce-backend`
service points its build context here. `make docker` builds `venapce-backend:latest`.

Set a real **`APP_SECRET_KEY`** in production — it keys the encryption of the
stored Superset password; rotating it invalidates that stored secret.

## Not yet (deliberate)

- **No app-level login** — the API is open behind CORS. App auth / per-space
  scoping (study §6) slots in as Fiber middleware later, when the front and
  FloMorphic side land.
- **Single space** — tables are space-agnostic for now; add a `space_id` column +
  scoping when multi-tenancy arrives.
