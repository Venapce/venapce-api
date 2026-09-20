// Package osquery is the venapce plugin's osquery/osctrl module. It exposes two
// actions — run a distributed osquery SQL against one node the user picks, or
// against every node carrying any of the tags they pick — plus the meta lookups
// that fill the node, tag and environment pickers on their forms.
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
	"log"
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
	// resultPageSize is how many result rows one results call fetches.
	resultPageSize = 200
	// resultMaxRows caps the rows a job commits, so a broad tag-targeted query
	// over a large fleet cannot balloon the flow scope. The output says when it
	// was hit.
	resultMaxRows = 5000
)

// Action method names. The forms' lookup buttons quote them (Field.Picks) so
// the environment picker, shared by both forms, knows which one to rebuild.
const (
	methodQuery       = "osquery.query"
	methodQueryByTags = "osquery.queryByTags"
)

// queryForm is kept as the built form so the node/env pickers can rebuild it with
// the discovered options spliced into the target field.
var queryForm = formkit.New("Run osquery").
	Describe("Dispatch an osquery SQL to one enrolled node and collect the rows it reports. The node answers on its next osctrl check-in, so this may take a few seconds.").
	Add(
		formkit.Text("env", "Environment").
			Describe("osctrl environment. Leave blank to use venapce's default. Press ↻ to list environments.").
			Lookup("osquery.meta.environments", "Environments").Picks(methodQuery),
		formkit.Text("uuid", "Node").Required().
			Describe("The enrolled node's UUID to query. Press ↻ to list nodes for the environment and pick one.").
			Lookup("osquery.meta.nodes", "Nodes"),
		formkit.TextArea("sql", "osquery SQL").Required().
			Describe("An osquery SQL statement, e.g. SELECT name, path, pid FROM processes LIMIT 20. Accepts {{$.path}} tokens."),
	).Build()

// queryByTagsForm targets by osctrl tag instead of by node. `tags` is an array
// property so the picker can re-render it as a multi-select (see
// flow.ChooseMany); until ↻ is pressed it is a plain add/remove list of names.
var queryByTagsForm = formkit.New("Run osquery by tags").
	Describe("Dispatch an osquery SQL to every active node carrying any of the selected osctrl tags (e.g. `linux`) and collect the rows they report. Nodes answer on their next check-in, so this may take a while for a large fleet.").
	Add(
		formkit.Text("env", "Environment").
			Describe("osctrl environment. Leave blank to use venapce's default. Press ↻ to list environments.").
			Lookup("osquery.meta.environments", "Environments").Picks(methodQueryByTags),
		formkit.List("tags", "Tags").Required().Set("uniqueItems", true).
			Describe("osctrl tag names to target; a node matching ANY of them is queried. Press ↻ to list the environment's tags and tick the ones you want.").
			Lookup("osquery.meta.tags", "Tags"),
		formkit.TextArea("sql", "osquery SQL").Required().
			Describe("An osquery SQL statement, e.g. SELECT name, path, pid FROM processes LIMIT 20. Accepts {{$.path}} tokens."),
	).Build()

// Actions returns the osquery module's actions, bound to venapce's osctrl.
func Actions(m *osctrl.Manager) []sdkv1.Action {
	return []sdkv1.Action{queryAction(m), queryByTagsAction(m)}
}

// Metas returns the picker lookups the query forms use.
func Metas(m *osctrl.Manager) []sdkv1.Meta {
	return []sdkv1.Meta{
		{Method: "osquery.meta.nodes", RequestHandler: metaNodes(m)},
		{Method: "osquery.meta.tags", RequestHandler: metaTags(m)},
		{Method: "osquery.meta.environments", RequestHandler: metaEnvironments(m)},
	}
}

// queryInput is the body of both query actions: osquery.query fills UUID,
// osquery.queryByTags fills Tags.
type queryInput struct {
	Env  string   `json:"env"`
	UUID string   `json:"uuid"`
	Tags []string `json:"tags"`
	SQL  string   `json:"sql"`
}

// resolveClientEnv is the preamble both actions share: the live osctrl client
// and the environment to run in (the form's, else venapce's default). It
// finishes the job with the right error and returns ok=false when either is
// missing.
func resolveClientEnv(job *sdkv1.Job, m *osctrl.Manager, in queryInput) (cl *osctrl.Client, env string, ok bool) {
	cl = m.Get()
	if cl == nil {
		job.DoneWithError("osctrl is not configured in venapce — connect it in Settings before running osquery")
		return nil, "", false
	}
	env = strings.TrimSpace(in.Env)
	if env == "" {
		env = cl.Environment()
	}
	if env == "" {
		job.DoneWithError("no environment: fill Environment on the node, or set a default osctrl environment in venapce Settings")
		return nil, "", false
	}
	if strings.TrimSpace(in.SQL) == "" {
		job.DoneWithError("missing required field: osquery SQL")
		return nil, "", false
	}
	return cl, env, true
}

func queryAction(m *osctrl.Manager) sdkv1.Action {
	return sdkv1.Action{
		Method:      methodQuery,
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

			cl, env, ok := resolveClientEnv(&job, m, in)
			if !ok {
				return
			}
			uuid := strings.TrimSpace(in.UUID)
			if uuid == "" {
				job.DoneWithError("missing required field: node (uuid) — press ↻ on the Node field to pick one")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
			defer cancel()
			runQuery(ctx, &job, cl, env, target{
				request: osctrl.DistributedQueryRequest{Query: in.SQL, UUIDList: []string{uuid}},
				output:  map[string]any{"uuid": uuid},
				waiting: "waiting for the node to report",
			})
		},
	}
}

func queryByTagsAction(m *osctrl.Manager) sdkv1.Action {
	return sdkv1.Action{
		Method:      methodQueryByTags,
		Title:       "Run osquery by tags",
		Description: "Dispatch an osquery SQL to every active node carrying any of the selected osctrl tags and return the rows they report.",
		Icon:        sdkv1.Icon{Icon: "mdi-tag-multiple"},
		Form:        queryByTagsForm,
		RequestHandler: func(job sdkv1.Job) {
			req, err := sdkv1.CastRequestTo[queryInput](job.Req.Data)
			if err != nil {
				job.DoneWithError("invalid request body: " + err.Error())
				return
			}
			in := req.Body
			flow.ResolveStruct(&job, &in)

			cl, env, ok := resolveClientEnv(&job, m, in)
			if !ok {
				return
			}
			tags := cleanTags(in.Tags)
			if len(tags) == 0 {
				job.DoneWithError("missing required field: tags — press ↻ on the Tags field and tick at least one")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
			defer cancel()

			// osctrl ignores a tag it does not know rather than matching zero
			// nodes, which would widen the target to the whole environment (see
			// osctrl.DistributedQueryRequest). Refuse to dispatch on a name the
			// environment does not have.
			raw, err := cl.Tags(ctx, env)
			if err != nil {
				job.DoneWithError("cannot verify tags for " + env + ": " + err.Error())
				return
			}
			if unknown := unknownTags(tags, tagOptions(raw)); len(unknown) > 0 {
				job.DoneWithError(fmt.Sprintf("unknown tag(s) in %s: %s — press ↻ on the Tags field to pick from the environment's tags", env, strings.Join(unknown, ", ")))
				return
			}

			runQuery(ctx, &job, cl, env, target{
				request: osctrl.DistributedQueryRequest{Query: in.SQL, TagList: tags},
				output:  map[string]any{"tags": tags},
				waiting: "waiting for tagged nodes to report",
			})
		},
	}
}

// target is what distinguishes the two actions once they reach the shared
// run→poll→collect cycle: the osctrl targeting to dispatch, the identifying
// fields to echo in the output, and the wording of the waiting frame.
type target struct {
	request osctrl.DistributedQueryRequest
	output  map[string]any
	waiting string
}

// runQuery dispatches the query, polls until every targeted node has reported (or
// the job times out), then collects and commits the rows.
func runQuery(ctx context.Context, job *sdkv1.Job, cl *osctrl.Client, env string, t target) {
	job.Progress(15, sdkv1.Frame{Title: "dispatching", Content: preview(t.request.Query)})

	name, err := cl.RunQuery(ctx, env, t.request)
	if err != nil {
		job.DoneWithError("dispatch failed: " + err.Error())
		return
	}

	job.Progress(35, sdkv1.Frame{Title: "waiting", Content: "query " + name + " — " + t.waiting})

	status := pollUntilDone(ctx, job, cl, env, name)

	job.Progress(85, sdkv1.Frame{Title: "collecting", Content: "reading results for " + name})
	rows, total, err := collectRows(ctx, cl, env, name)
	if err != nil {
		job.DoneWithError("collecting results failed: " + err.Error())
		return
	}
	// osctrl bumps the query's counters before it has written the node's
	// result row (the write is a goroutine), so a read right after the status
	// flips to done can land a row short — typically the one carrying an error
	// message. Give it one more chance to appear before reporting.
	if status != nil && total < status.Executions+status.Errors {
		select {
		case <-ctx.Done():
		case <-time.After(pollEvery):
			if again, againTotal, err := collectRows(ctx, cl, env, name); err == nil {
				rows, total = again, againTotal
			}
		}
	}
	decodeRowData(rows)
	rows, failures := splitFailures(rows)

	// rows are per-node envelopes; the osquery rows themselves sit in each
	// envelope's data.result, so count those too — a node that ran the SQL
	// and matched nothing still contributes one envelope with an empty result.
	matched := resultRowCount(rows)
	out := map[string]any{
		"queryName":  name,
		"env":        env,
		"rows":       rows,
		"rowCount":   len(rows),
		"resultRows": matched,
		"columns":    columnsOf(rows),
	}
	if total > len(rows)+len(failures) {
		out["totalRows"] = total
		out["truncated"] = true
	}
	for k, v := range t.output {
		out[k] = v
	}
	// How many nodes ran the SQL and how many rejected it. osctrl's counters
	// are authoritative when the status read worked; otherwise fall back to
	// what the result rows say.
	succeeded, failed := len(rows), len(failures)
	if status != nil {
		succeeded, failed = status.Executions, status.Errors
		out["completed"] = status.Done()
		out["expected"] = status.Expected
		out["executions"] = status.Executions
		// osctrl's own record of the query, verbatim, so the context shows
		// exactly what the osctrl panel shows for it.
		var record any
		if json.Unmarshal(status.Raw, &record) == nil && record != nil {
			out["osctrlQuery"] = record
		}
	}
	out["errors"] = failed
	if len(failures) > 0 {
		out["failures"] = failures
	}
	// A query every node rejected is a failed action, not a result with zero
	// rows: fail the job with osquery's own message (typically a bad table or
	// column) so a flow or assistant reading the context sees why, instead of
	// a green run whose only symptom is a non-zero `errors` count.
	if succeeded == 0 && failed > 0 {
		finish(job, name, func() any { return job.DoneWithErrorData(failureSummary(failed, failures), out) })
		return
	}
	switch {
	case status != nil && status.Expected == 0:
		out["note"] = "the target resolved to no active nodes; nothing was queried"
	case failed > 0:
		out["note"] = fmt.Sprintf("%d node(s) failed to run the query; see failures", failed)
	case status != nil && !status.Done():
		out["note"] = "not all targeted nodes had reported before the timeout; rows are what arrived so far"
	case succeeded > 0 && matched == 0:
		out["note"] = fmt.Sprintf("the query ran on %d node(s) and matched no rows", succeeded)
	}
	finish(job, name, func() any { return job.Done(out) })
}

// finish sends the job's final command and refuses to let a failed send pass
// silently: the SDK returns the transport error instead of raising it, and a
// job whose Done never reached the runtime leaves the node with an empty
// context and no hint why. On failure it logs and retries once, then ends the
// job with an error carrying the reason so the flow at least sees that.
func finish(job *sdkv1.Job, name string, send func() any) {
	err, failed := send().(error)
	if !failed {
		return
	}
	log.Printf("osquery: job %s (%s): final command not delivered: %v — retrying", job.JobId, name, err)
	if err, failed = send().(error); !failed {
		return
	}
	log.Printf("osquery: job %s (%s): final command failed again: %v", job.JobId, name, err)
	job.DoneWithError("query " + name + " ran, but its result could not be delivered to the flow runtime: " + err.Error())
}

// splitFailures separates the result rows that are really execution errors from
// the data. osctrl records a node's failed run as a row like any other, with
// `status` != 0 and osquery's message inside the decoded data envelope
// ({"result":[],"status":1,"message":"no such table: …"}), so left in `rows` it
// reads as one more result. Each failure is returned as {uuid, status, message}.
func splitFailures(rows []map[string]any) (ok []map[string]any, failures []map[string]any) {
	for _, row := range rows {
		st := rowStatus(row)
		if st == 0 {
			ok = append(ok, row)
			continue
		}
		f := map[string]any{"uuid": str(row["uuid"]), "status": st}
		if data, isMap := row["data"].(map[string]any); isMap {
			if msg := str(data["message"]); msg != "" {
				f["message"] = msg
			}
		}
		failures = append(failures, f)
	}
	if ok == nil {
		ok = []map[string]any{}
	}
	return ok, failures
}

// resultRowCount sums the osquery rows inside the envelopes' decoded
// data.result arrays. Envelopes whose data is not decoded (or has no result
// array) add nothing.
func resultRowCount(rows []map[string]any) int {
	n := 0
	for _, row := range rows {
		if data, ok := row["data"].(map[string]any); ok {
			if result, ok := data["result"].([]any); ok {
				n += len(result)
			}
		}
	}
	return n
}

// rowStatus reads a result row's osquery exit status: the top-level `status`
// osctrl stores next to the row, else the one inside the decoded data envelope.
// JSON numbers arrive as float64; anything unreadable counts as success.
func rowStatus(row map[string]any) int {
	if st, ok := numInt(row["status"]); ok {
		return st
	}
	if data, ok := row["data"].(map[string]any); ok {
		if st, ok := numInt(data["status"]); ok {
			return st
		}
	}
	return 0
}

func numInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// failureSummary is the job's error line when every node rejected the query:
// the count plus the distinct messages osquery gave, so the reason (a bad table
// or column, usually) is readable without opening the payload.
func failureSummary(failed int, failures []map[string]any) string {
	seen := map[string]bool{}
	var msgs []string
	for _, f := range failures {
		m := str(f["message"])
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		msgs = append(msgs, m)
	}
	head := fmt.Sprintf("osquery failed on %d node(s)", failed)
	if len(msgs) == 0 {
		return head + ": osctrl recorded no error message; check the query in the osctrl panel"
	}
	return head + ": " + strings.Join(msgs, "; ")
}

// collectRows pages through a query's results until osctrl has no more pages or
// resultMaxRows is reached. It returns the rows plus osctrl's total count, so the
// caller can tell when the cap cut the set short.
func collectRows(ctx context.Context, cl *osctrl.Client, env, name string) ([]map[string]any, int, error) {
	var rows []map[string]any
	total := 0
	for page := 1; ; page++ {
		res, err := cl.QueryResults(ctx, env, name, page, resultPageSize, "")
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, res.Items...)
		total = res.TotalItems
		if len(res.Items) == 0 || page >= res.TotalPages || len(rows) >= resultMaxRows {
			break
		}
	}
	if len(rows) > resultMaxRows {
		rows = rows[:resultMaxRows]
	}
	if total < len(rows) {
		total = len(rows)
	}
	return rows, total, nil
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
			// osctrl commits the query with its target set already resolved, so
			// Expected==0 on the record means nothing matched (e.g. tags with no
			// active nodes) — there is nobody to wait for.
			if st.Expected == 0 {
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

// metaTags backs the Tags multi-select: list the environment's tags and
// re-render the form with the tags field as a multi-select of them. Whatever
// the user has already ticked is echoed back so it stays selected.
func metaTags(m *osctrl.Manager) func(sdkv1.Request) any {
	return func(req sdkv1.Request) any {
		call := flow.DecodeMeta[map[string]any](req.Data)
		cl := m.Get()
		if cl == nil {
			return formkit.Failure("osctrl is not configured in venapce — connect it in Settings").About("tags").Patch(nil)
		}
		env := flow.MetaString(call, "env")
		if env == "" {
			env = cl.Environment()
		}
		if env == "" {
			return formkit.Warning("pick or type an Environment first, then press ↻ to list its tags").About("tags").Patch(nil)
		}

		ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
		defer cancel()
		raw, err := cl.Tags(ctx, env)
		if err != nil {
			return formkit.Failure("cannot list tags for %s: %s", env, err).About("tags").Patch(nil)
		}
		options := tagOptions(raw)
		if len(options) == 0 {
			return formkit.Warning("no tags in %s", env).About("tags").Patch(nil)
		}
		heading := formkit.Success("%d tag(s) in %s — tick the ones to target", len(options), env).About("tags")
		return flow.ChooseMany(queryByTagsForm, "tags", options, formkit.FormData(call), heading)
	}
}

// metaEnvironments backs the Environment picker on both forms. The button says
// which action's form it sits on (`form`, from Field.Picks); that is the form
// re-rendered with the drop-down — rebuilding the wrong one would swap the
// dialog to the other action.
func metaEnvironments(m *osctrl.Manager) func(sdkv1.Request) any {
	return func(req sdkv1.Request) any {
		call := flow.DecodeMeta[map[string]any](req.Data)
		form := formFor(flow.MetaString(call, "form"))
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
		return formkit.Choose(form, "env", options, formkit.FormData(call), heading)
	}
}

// formFor maps an action method to its built form, defaulting to the per-node
// query form for an older button that names none.
func formFor(method string) sdkv1.FormBuilder {
	if method == methodQueryByTags {
		return queryByTagsForm
	}
	return queryForm
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

// tagOptions turns an osctrl tag array into picker options keyed by the tag
// name (what tag_list expects). The label adds the description when there is
// one, and marks tags osctrl created itself (auto_tag) so a user can tell
// `linux` the platform tag from `linux` the hand-made one.
func tagOptions(raw json.RawMessage) []formkit.Option {
	var tags []map[string]any
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil
	}
	options := make([]formkit.Option, 0, len(tags))
	for _, t := range tags {
		name := str(t["name"])
		if name == "" {
			continue
		}
		label := name
		if d := str(t["description"]); d != "" && d != name {
			label += " · " + d
		}
		if auto, _ := t["auto_tag"].(bool); auto {
			label += " (auto)"
		}
		options = append(options, formkit.Option{Value: name, Label: label})
	}
	return options
}

// cleanTags trims and de-duplicates the tag names a form submits, dropping
// blanks, so the target list osctrl sees is exactly what the user meant.
func cleanTags(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// unknownTags returns the requested names that are not among the environment's
// tags, in request order.
func unknownTags(requested []string, known []formkit.Option) []string {
	have := make(map[string]bool, len(known))
	for _, o := range known {
		have[o.Value] = true
	}
	var missing []string
	for _, t := range requested {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	return missing
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
