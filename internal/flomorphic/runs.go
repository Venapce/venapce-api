package flomorphic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The run side of the FloMorphic API: the flows an operator can pick, the
// context document a run reads and writes, and the process (run) itself. This
// is what lets venapce send one of its own rows through a flow and collect
// what the flow concluded — see internal/httpapi/activities.go.

// Flow is the subset of a FloMorphic workflow row a picker needs. The graph
// itself (view_flow) is not carried.
type Flow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// page is FloMorphic's page envelope: { list, total, page, per_page, total_pages }.
type page[T any] struct {
	List  []T   `json:"list"`
	Total int64 `json:"total"`
}

// ListFlows returns up to perPage flows matching search (newest first as
// FloMorphic orders them). A search shorter than FloMorphic's minimum is sent
// as-is and FloMorphic answers 400, which surfaces as an error.
func (c *Client) ListFlows(ctx context.Context, search string, perPage int) ([]Flow, error) {
	if perPage <= 0 {
		perPage = 100
	}
	q := url.Values{"per_page": {strconv.Itoa(perPage)}}
	if s := strings.TrimSpace(search); s != "" {
		q.Set("search", s)
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
	IndexID    int64          `json:"indexId"`
	PID        string         `json:"pid"`
	FlowID     string         `json:"flowId"`
	ContextID  string         `json:"contextId"`
	Status     string         `json:"status"`
	Error      string         `json:"error"`
	Meta       map[string]any `json:"meta"`
	Snapshot   map[string]any `json:"snapshot"`
	StartedAt  int64          `json:"startedAt"`
	FinishedAt int64          `json:"finishedAt"`
	DurationMs int64          `json:"durationMs"`
}

// StartProcess launches flowID on the context document contextID and returns
// the recorded process. meta is venapce's record meta on the run (how the run
// can be traced back to its activity from the FloMorphic side). A launch that
// FloMorphic recorded as failed still returns the row alongside the error.
func (c *Client) StartProcess(ctx context.Context, flowID, contextID string, meta map[string]any) (*Process, error) {
	body := map[string]any{"flowId": flowID, "contextId": contextID, "meta": meta}
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
