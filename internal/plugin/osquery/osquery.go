// Package osquery is the venapce plugin's osquery/osctrl module. It exposes one
// action — run a distributed osquery SQL against a node the user picks — plus the
// meta lookups that fill the node and environment pickers on its form.
//
// It carries no connection settings: the live osctrl client is venapce's own
// (internal/osctrl), injected as a Manager. osctrl distributed queries are
// asynchronous, so the action models the whole run → poll → collect cycle inside
// one long-lived job.
package osquery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"

	"github.com/Venapce/venapce-api/internal/osctrl"
	"github.com/Venapce/venapce-api/internal/plugin/flow"
)

const (
	// jobTimeout bounds the whole run→poll→collect cycle. A distributed query
	// only completes once its target nodes check in, so this is generous.
	jobTimeout = 3 * time.Minute
	// pollEvery is how often the job re-checks a dispatched query's status.
	pollEvery = 2 * time.Second
	// metaTimeout keeps the form's picker lookups snappy.
	metaTimeout = 15 * time.Second
	// resultPageSize caps how many result rows one collect returns.
	resultPageSize = 100
)

// queryForm is kept as the built form so the node/env pickers can rebuild it with
// the discovered options spliced into the target field.
var queryForm = formkit.New("Run osquery").
	Describe("Dispatch an osquery SQL to one enrolled node and collect the rows it reports. The node answers on its next osctrl check-in, so this may take a few seconds.").
	Add(
		formkit.Text("env", "Environment").
			Describe("osctrl environment. Leave blank to use venapce's default. Press ↻ to list environments.").
			Lookup("osquery.meta.environments", "Environments"),
		formkit.Text("uuid", "Node").Required().
			Describe("The enrolled node's UUID to query. Press ↻ to list nodes for the environment and pick one.").
			Lookup("osquery.meta.nodes", "Nodes"),
		formkit.TextArea("sql", "osquery SQL").Required().
			Describe("An osquery SQL statement, e.g. SELECT name, path, pid FROM processes LIMIT 20. Accepts {{$.path}} tokens."),
	).Build()

// Actions returns the osquery module's action(s), bound to venapce's osctrl.
func Actions(m *osctrl.Manager) []sdkv1.Action {
	return []sdkv1.Action{queryAction(m)}
}

// Metas returns the picker lookups the query form uses.
func Metas(m *osctrl.Manager) []sdkv1.Meta {
	return []sdkv1.Meta{
		{Method: "osquery.meta.nodes", RequestHandler: metaNodes(m)},
		{Method: "osquery.meta.environments", RequestHandler: metaEnvironments(m)},
	}
}

type queryInput struct {
	Env  string `json:"env"`
	UUID string `json:"uuid"`
	SQL  string `json:"sql"`
}

func queryAction(m *osctrl.Manager) sdkv1.Action {
	return sdkv1.Action{
		Method:      "osquery.query",
		Title:       "Run osquery on a node",
		Description: "Dispatch an osquery SQL to one enrolled node via osctrl and return the rows it reports.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-search"},
		Form:        queryForm,
		RequestHandler: func(job sdkv1.Job) {
			req, err := sdkv1.CastRequestTo[queryInput](job.Req.Data)
			if err != nil {
				job.DoneWithError("invalid request body: " + err.Error())
				return
			}
			in := req.Body
			flow.ResolveStruct(&job, &in)

			cl := m.Get()
			if cl == nil {
				job.DoneWithError("osctrl is not configured in venapce — connect it in Settings before running osquery")
				return
			}
			env := strings.TrimSpace(in.Env)
			if env == "" {
				env = cl.Environment()
			}
			if env == "" {
				job.DoneWithError("no environment: fill Environment on the node, or set a default osctrl environment in venapce Settings")
				return
			}
			if strings.TrimSpace(in.UUID) == "" {
				job.DoneWithError("missing required field: node (uuid) — press ↻ on the Node field to pick one")
				return
			}
			if strings.TrimSpace(in.SQL) == "" {
				job.DoneWithError("missing required field: osquery SQL")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
			defer cancel()
			runQuery(ctx, &job, cl, env, in)
		},
	}
}

// runQuery dispatches the query, polls until every targeted node has reported (or
// the job times out), then collects and commits the rows.
func runQuery(ctx context.Context, job *sdkv1.Job, cl *osctrl.Client, env string, in queryInput) {
	job.Progress(15, sdkv1.Frame{Title: "dispatching", Content: preview(in.SQL)})

	name, err := cl.RunQuery(ctx, env, osctrl.DistributedQueryRequest{
		Query:    in.SQL,
		UUIDList: []string{strings.TrimSpace(in.UUID)},
	})
	if err != nil {
		job.DoneWithError("dispatch failed: " + err.Error())
		return
	}

	job.Progress(35, sdkv1.Frame{Title: "waiting", Content: "query " + name + " — waiting for the node to report"})

	status := pollUntilDone(ctx, job, cl, env, name)

	job.Progress(85, sdkv1.Frame{Title: "collecting", Content: "reading results for " + name})
	res, err := cl.QueryResults(ctx, env, name, 1, resultPageSize, "")
	if err != nil {
		job.DoneWithError("collecting results failed: " + err.Error())
		return
	}
	decodeRowData(res.Items)

	out := map[string]any{
		"queryName": name,
		"env":       env,
		"uuid":      strings.TrimSpace(in.UUID),
		"rows":      res.Items,
		"rowCount":  len(res.Items),
		"columns":   columnsOf(res.Items),
	}
	if status != nil {
		out["completed"] = status.Done()
		out["expected"] = status.Expected
		out["executions"] = status.Executions
		out["errors"] = status.Errors
		if !status.Done() {
			out["note"] = "not all targeted nodes had reported before the timeout; rows are what arrived so far"
		}
	}
	job.Done(out)
}

// pollUntilDone re-reads the query's status until osctrl marks it done or the
// context expires. It returns the last status it saw (possibly nil if the very
// first read failed), so the caller can still collect whatever rows arrived.
func pollUntilDone(ctx context.Context, job *sdkv1.Job, cl *osctrl.Client, env, name string) *osctrl.DistributedQuery {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	var last *osctrl.DistributedQuery
	for {
		st, err := cl.QueryStatus(ctx, env, name)
		if err == nil {
			last = st
			if st.Done() {
				return last
			}
			pct := 35
			if st.Expected > 0 {
				pct = 35 + (st.Executions+st.Errors)*45/st.Expected
				if pct > 80 {
					pct = 80
				}
			}
			job.Progress(pct, sdkv1.Frame{Title: "waiting", Content: fmt.Sprintf("%d/%d nodes reported", st.Executions+st.Errors, st.Expected)})
		}
		select {
		case <-ctx.Done():
			return last
		case <-ticker.C:
		}
	}
}

// metaNodes backs the Node picker: list the environment's enrolled nodes and
// re-render the form with the uuid field as a drop-down of them.
func metaNodes(m *osctrl.Manager) func(sdkv1.Request) any {
	return func(req sdkv1.Request) any {
		call := flow.DecodeMeta[map[string]any](req.Data)
		cl := m.Get()
		if cl == nil {
			return formkit.Failure("osctrl is not configured in venapce — connect it in Settings").About("uuid").Patch(nil)
		}
		env := flow.MetaString(call, "env")
		if env == "" {
			env = cl.Environment()
		}
		if env == "" {
			return formkit.Warning("pick or type an Environment first, then press ↻ to list its nodes").About("uuid").Patch(nil)
		}

		ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
		defer cancel()
		raw, err := cl.Nodes(ctx, env)
		if err != nil {
			return formkit.Failure("cannot list nodes for %s: %s", env, err).About("uuid").Patch(nil)
		}
		options := nodeOptions(raw)
		if len(options) == 0 {
			return formkit.Warning("no enrolled nodes in %s", env).About("uuid").Patch(nil)
		}
		heading := formkit.Success("%d node(s) in %s — pick one", len(options), env).About("uuid")
		return formkit.Choose(queryForm, "uuid", options, formkit.FormData(call), heading)
	}
}

// metaEnvironments backs the Environment picker.
func metaEnvironments(m *osctrl.Manager) func(sdkv1.Request) any {
	return func(req sdkv1.Request) any {
		call := flow.DecodeMeta[map[string]any](req.Data)
		cl := m.Get()
		if cl == nil {
			return formkit.Failure("osctrl is not configured in venapce — connect it in Settings").About("env").Patch(nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
		defer cancel()
		raw, err := cl.Environments(ctx)
		if err != nil {
			return formkit.Failure("cannot list environments: %s", err).About("env").Patch(nil)
		}
		options := envOptions(raw)
		if len(options) == 0 {
			return formkit.Warning("no osctrl environments found").About("env").Patch(nil)
		}
		heading := formkit.Success("%d environment(s) — pick one", len(options)).About("env")
		return formkit.Choose(queryForm, "env", options, formkit.FormData(call), heading)
	}
}

// nodeOptions turns an osctrl node array into picker options: the UUID is the
// value, and a human label pairs the hostname (or localname) with the platform.
func nodeOptions(raw json.RawMessage) []formkit.Option {
	var nodes []map[string]any
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil
	}
	options := make([]formkit.Option, 0, len(nodes))
	for _, n := range nodes {
		uuid := str(n["uuid"])
		if uuid == "" {
			continue
		}
		name := firstNonEmpty(str(n["localname"]), str(n["hostname"]), uuid)
		label := name
		if p := str(n["platform"]); p != "" {
			label += " · " + p
		}
		options = append(options, formkit.Option{Value: uuid, Label: label})
	}
	return options
}

// envOptions turns an osctrl environment array into picker options keyed by the
// environment name (what the query routes expect).
func envOptions(raw json.RawMessage) []formkit.Option {
	var envs []map[string]any
	if err := json.Unmarshal(raw, &envs); err != nil {
		return nil
	}
	options := make([]formkit.Option, 0, len(envs))
	for _, e := range envs {
		name := str(e["name"])
		if name == "" {
			continue
		}
		label := name
		if h := str(e["hostname"]); h != "" {
			label += " · " + h
		}
		options = append(options, formkit.Option{Value: name, Label: label})
	}
	return options
}

// decodeRowData unwraps each result row's osctrl-encoded "data" field. osctrl
// stores the osquery result envelope ({"name","result":[…],"status","message"})
// as a JSON *string*, so a raw row carries it as an opaque string that downstream
// flow nodes cannot index into. Where "data" is a string holding valid JSON, it
// is replaced in place with the decoded value so the row presents as structured
// JSON. A non-string or non-JSON "data" is left untouched.
func decodeRowData(rows []map[string]any) {
	for _, row := range rows {
		s, ok := row["data"].(string)
		if !ok {
			continue
		}
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) == nil {
			row["data"] = decoded
		}
	}
}

// columnsOf collects the union of keys across result rows, so downstream nodes
// know the shape even when a row omits a null column. Order is not guaranteed by
// osquery, so it is left as encountered.
func columnsOf(rows []map[string]any) []string {
	seen := map[string]bool{}
	var cols []string
	for _, row := range rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	return cols
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// preview trims a statement to one short line for a progress frame.
func preview(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > 120 {
		return sql[:117] + "…"
	}
	return sql
}
