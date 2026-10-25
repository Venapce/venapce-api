package flomorphic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	tagutil "github.com/Venapce/venapce-api/internal/tags"
)

// The run side of the FloMorphic API: the flows an operator can pick, the
// context document a run reads and writes, and the process (run) itself. This
// is what lets venapce send one of its own rows through a flow and collect
// what the flow concluded — see internal/httpapi/activities.go.

// Flow is the subset of a FloMorphic workflow row a picker needs. The graph
// itself (view_flow) is not carried.
//
// Tags are the flow's labels on FloMorphic — what an operation stamps on the
// flows it installs, and what the run picker narrows by so an operator sees the
// handful their operation brought instead of every flow on the canvas.
type Flow struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	// Preferences are the flow's non-graph choices — today the entry form it
	// asks to have filled in before it runs (see entry.go). Carried on the
	// list so the run picker can mark the flows that ask for input without a
	// round trip per row; the form's starting values still come from
	// FlowEntry, which is the one thing that resolves them.
	Preferences *FlowPreferences `json:"preferences,omitempty"`
	CreatedAt   int64            `json:"createdAt"`
	UpdatedAt   int64            `json:"updatedAt"`
}

// Entry is the form this flow asks to have filled in, or nil when it asks for
// nothing — the picker's "does this flow need input" test.
func (f Flow) Entry() *EntryForm { return f.Preferences.EntryOrNil() }

// page is FloMorphic's page envelope: { list, total, page, per_page, total_pages }.
type page[T any] struct {
	List  []T   `json:"list"`
	Total int64 `json:"total"`
}

// ListFlows returns up to perPage flows matching search (newest first as
// FloMorphic orders them). A search shorter than FloMorphic's minimum is sent
// as-is and FloMorphic answers 400, which surfaces as an error.
//
// tags is a match-ANY label filter applied by FloMorphic, not here: a flow is
// kept when it carries at least one of them. Empty means no filter — the whole
// list, which is what the picker shows until someone narrows it, and what it
// shows again the moment they clear the filter.
func (c *Client) ListFlows(ctx context.Context, search string, tags []string, perPage int) ([]Flow, error) {
	if perPage <= 0 {
		perPage = 100
	}
	q := url.Values{"per_page": {strconv.Itoa(perPage)}}
	if s := strings.TrimSpace(search); s != "" {
		q.Set("search", s)
	}
	if clean := tagutil.Clean(tags); len(clean) > 0 {
		q.Set("tags", strings.Join(clean, ","))
	}
	data, err := c.do(ctx, http.MethodGet, "/flow?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var p page[Flow]
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid flow list: %w", err)
	}
	if p.List == nil {
		p.List = []Flow{}
	}
	return p.List, nil
}

// GetFlow reads one flow's identity (to record its title on a run).
func (c *Client) GetFlow(ctx context.Context, id string) (Flow, error) {
	data, err := c.do(ctx, http.MethodGet, "/flow/id/"+url.PathEscape(id), nil)
	if err != nil {
		return Flow{}, err
	}
	var f Flow
	if err := json.Unmarshal(data, &f); err != nil {
		return Flow{}, fmt.Errorf("invalid flow: %w", err)
	}
	return f, nil
}

// ContextDoc is a FloMorphic context document: the JSON object a run reads
// its input from and writes its state into. `Context` is that object,
// serialized (that is how FloMorphic stores and returns it).
type ContextDoc struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Context   string         `json:"context"`
	Header    map[string]any `json:"header"`
	UpdatedAt int64          `json:"updatedAt"`
}

// Document decodes the serialized context object; an empty or unparsable
// body yields an empty object rather than an error, since a run that wrote
// nothing is not a failure.
func (d ContextDoc) Document() map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(d.Context) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(d.Context), &out)
	return out
}

// CreateContext stores a new context document holding doc (must marshal to a
// JSON object) and returns its id.
func (c *Client) CreateContext(ctx context.Context, title string, doc map[string]any) (string, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal context: %w", err)
	}
	body := map[string]any{"title": title, "context": string(b), "header": map[string]any{}}
	data, err := c.do(ctx, http.MethodPost, "/context", body)
	if err != nil {
		return "", err
	}
	var rec ContextDoc
	if err := json.Unmarshal(data, &rec); err != nil {
		return "", fmt.Errorf("invalid context response: %w", err)
	}
	if rec.ID == "" {
		return "", fmt.Errorf("FloMorphic returned a context with no id")
	}
	return rec.ID, nil
}

// GetContext reads a context document back — after a run, this is where the
// flow's output lives.
func (c *Client) GetContext(ctx context.Context, id string) (ContextDoc, error) {
	data, err := c.do(ctx, http.MethodGet, "/context/id/"+url.PathEscape(id), nil)
	if err != nil {
		return ContextDoc{}, err
	}
	var rec ContextDoc
	if err := json.Unmarshal(data, &rec); err != nil {
		return ContextDoc{}, fmt.Errorf("invalid context: %w", err)
	}
	return rec, nil
}

// DeleteContext removes a context document; a missing one is not an error.
func (c *Client) DeleteContext(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	_, err := c.do(ctx, http.MethodDelete, "/context/id/"+url.PathEscape(id), nil)
	var he *HTTPError
	if err != nil && asHTTP(err, &he) && he.NotFound() {
		return nil
	}
	return err
}

// Process statuses as FloMorphic reports them (models.ProcessStatus).
const (
	ProcessScheduled = "scheduled"
	ProcessRunning   = "running"
	ProcessWaiting   = "waiting"
	ProcessFinished  = "finished"
	ProcessStopped   = "stopped"
	ProcessFailed    = "failed"
)

// ProcessOpen reports whether a status is still in flight (worth polling).
func ProcessOpen(status string) bool {
	switch status {
	case ProcessScheduled, ProcessRunning, ProcessWaiting:
		return true
	}
	return false
}

// Process is one run of a flow, as FloMorphic records it. IndexID is the run's
// integer identity (the process id venapce stores); PID is the engine's uuid.
type Process struct {
	IndexID   int64          `json:"indexId"`
	PID       string         `json:"pid"`
	FlowID    string         `json:"flowId"`
	ContextID string         `json:"contextId"`
	Status    string         `json:"status"`
	Error     string         `json:"error"`
	Meta      map[string]any `json:"meta"`
	Snapshot  map[string]any `json:"snapshot"`
	// Errors is the ledger of everything that went wrong during this run — see
	// Ledger, below. Absent for a run that hit nothing.
	Errors     *RunErrors `json:"errors,omitempty"`
	StartedAt  int64      `json:"startedAt"`
	FinishedAt int64      `json:"finishedAt"`
	DurationMs int64      `json:"durationMs"`
}

// StartProcess launches flowID on the context document contextID and returns
// the recorded process. meta is venapce's record meta on the run (how the run
// can be traced back to its activity from the FloMorphic side). A launch that
// FloMorphic recorded as failed still returns the row alongside the error.
//
// entry are the values filled into the flow's entry form (see entry.go), or nil
// when the flow declares none — or when nothing was supplied for an optional
// one, which FloMorphic answers by seeding the form's defaults. The values are
// validated against the flow's own schema and seeded into the context before
// the run is dispatched, so a launch that does not satisfy the flow's input
// contract fails here with the engine's own message rather than halfway through
// the graph.
func (c *Client) StartProcess(ctx context.Context, flowID, contextID string, meta, entry map[string]any) (*Process, error) {
	body := map[string]any{"flowId": flowID, "contextId": contextID, "meta": meta}
	if len(entry) > 0 {
		body["entry"] = entry
	}
	data, err := c.do(ctx, http.MethodPost, "/process", body)
	if err != nil {
		return nil, err
	}
	var p Process
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid process response: %w", err)
	}
	return &p, nil
}

// GetProcess reads a run's current state.
func (c *Client) GetProcess(ctx context.Context, indexID int64) (*Process, error) {
	data, err := c.do(ctx, http.MethodGet, "/process/id/"+strconv.FormatInt(indexID, 10), nil)
	if err != nil {
		return nil, err
	}
	var p Process
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid process: %w", err)
	}
	return &p, nil
}

// StopProcess asks the engine to stop a run.
func (c *Client) StopProcess(ctx context.Context, indexID int64) (*Process, error) {
	data, err := c.do(ctx, http.MethodPost, "/process/id/"+strconv.FormatInt(indexID, 10)+"/stop", struct{}{})
	if err != nil {
		return nil, err
	}
	var p Process
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid process: %w", err)
	}
	return &p, nil
}

// ---- the run's error ledger ----

// A node error does not stop a flow: fractal records it, writes it into the
// run's context header under `_errors`, and carries on. FloMorphic lifts that
// header onto the process row (per pid, since overlapping runs of one context
// document would clobber each other). So a run can report `finished` and still
// have hit a dozen things, and this ledger is the only place a caller that was
// not watching the event stream can read that from — without it a run that
// failed at five nodes and still completed is indistinguishable from a clean
// one. Venapce keeps it on the activity so every view of a run can say so.

// Error kinds, as fractal stamps them (models.ErrorKind).
const (
	// ErrorKindNode is the flow's own: its node data, its js or rego, or
	// something its node called that did not deliver. The flow author can act
	// on it.
	ErrorKindNode = "node"
	// ErrorKindSystem is the platform's: infra that would not answer, a reply
	// that would not marshal. Nothing in the flow caused it and nothing in the
	// flow mends it.
	ErrorKindSystem = "system"
)

// NodeError is one entry of the ledger (fractal-core models.NodeError).
type NodeError struct {
	Ts   int64  `json:"ts"`
	Kind string `json:"kind"`
	// Flow the error was raised in — a run that went through a GoTo has entries
	// from more than one.
	Flow string `json:"flow"`
	Node string `json:"node"`
	// Src is the actor that raised it — rt / js / rego / plugin:<title>.
	Src string `json:"src"`
	// Code is the engine status code (fractal StatusFractal).
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	// Loc is the JSON path the failure happened on, set only for a node running
	// over a fan-out where the node id alone does not place it.
	Loc string `json:"loc,omitempty"`
}

// RunErrors is one run's ledger. Count is the true total and Items may be
// shorter: the header rides inside the context document, which has a publish
// limit, so a cascading run's entries are capped at the earliest — which is
// where the cause of a cascade is.
type RunErrors struct {
	Pid   string      `json:"pid"`
	Count int         `json:"count"`
	Items []NodeError `json:"items"`
}

// KindCounts splits the carried items by kind. Counted over Items rather than
// Count, because the cap drops the tail and only a split derived from what is
// in hand adds up to what is shown.
func (e RunErrors) KindCounts() (node, system int) {
	for _, it := range e.Items {
		if it.Kind == ErrorKindSystem {
			system++
			continue
		}
		node++
	}
	return node, system
}

// Summary is the one-line form a caller shows next to the run's status —
// "3 errors recorded during this run · 2 node, 1 system".
func (e RunErrors) Summary() string {
	node, system := e.KindCounts()
	head := fmt.Sprintf("%d error%s recorded during this run", e.Count, plural(e.Count))
	parts := []string{}
	if node > 0 {
		parts = append(parts, fmt.Sprintf("%d node", node))
	}
	if system > 0 {
		parts = append(parts, fmt.Sprintf("%d system", system))
	}
	if len(parts) == 0 {
		return head
	}
	return head + " · " + strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Ledger is the run's errors, or nil when there is nothing to report.
func (p *Process) Ledger() *RunErrors {
	if p == nil {
		return nil
	}
	return p.Errors.Normalized(p.PID)
}

// HeaderErrorsKey is the context-document header slot the engine stamps the
// ledger into, before FloMorphic lifts it onto the process row.
const HeaderErrorsKey = "_errors"

// LedgerFromHeader reads the ledger straight off a context document's header.
//
// The process row is the place to read it from — it is kept per pid, while the
// context row is shared and overlapping runs of one document clobber each
// other's header. This is the fallback for a FloMorphic that stores the header
// but not yet the process column, so an upgrade of one half of the pair does
// not lose the ledger.
func LedgerFromHeader(header map[string]any) *RunErrors {
	raw, ok := header[HeaderErrorsKey]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var e RunErrors
	if err := json.Unmarshal(b, &e); err != nil {
		return nil
	}
	return e.Normalized("")
}

// Normalized fills in what an engine may have left out and reports nil when the
// ledger says nothing happened.
//
// Presence of the field is not the test: a clean run carries no ledger at all,
// and an engine that predates it answers with an empty object. Having a count
// is — and a ledger whose count was not filled in is counted from its items, so
// a short answer is still reported rather than dropped.
func (e *RunErrors) Normalized(pid string) *RunErrors {
	if e == nil {
		return nil
	}
	out := *e
	if out.Items == nil {
		out.Items = []NodeError{}
	}
	if out.Count == 0 {
		out.Count = len(out.Items)
	}
	if out.Count == 0 {
		return nil
	}
	if out.Pid == "" {
		out.Pid = pid
	}
	return &out
}
