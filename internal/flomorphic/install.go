package flomorphic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	tagutil "github.com/Venapce/venapce-api/internal/tags"
)

// The install side of the FloMorphic API — what venapce's operation packages
// need beyond listing and running flows: landing a portable workflow export
// as a flow (POST /flow/import, the REST twin of the editor's Import dialog)
// and knowing which plugin actions this FloMorphic can actually run (the
// extension table's action rows), for the readiness checks.

// MissingAction is one plugin action a workflow file calls that no plugin on
// the FloMorphic install provides, with what the file knew about where to get
// it.
type MissingAction struct {
	Action string   `json:"action"`
	Plugin string   `json:"plugin"`
	Repo   string   `json:"repo,omitempty"`
	Ref    string   `json:"ref,omitempty"`
	Subdir string   `json:"subdir,omitempty"`
	Nodes  []string `json:"nodes"`
}

// UnboundNode is one plugin node the import kept but did not bind, because
// binding it would have been a guess: several plugins registered on the
// FloMorphic serve its action and the document named none of them.
//
// Not a missing plugin — the action is served, more than once over — so it
// arrives apart from MissingActions, whose message is "install this plugin".
// The node lands flagged on the canvas and the pick is made in its drawer,
// against the plugins named in Choices.
type UnboundNode struct {
	Ref     string   `json:"ref"`
	Title   string   `json:"title,omitempty"`
	Action  string   `json:"action"`
	Plugin  string   `json:"plugin,omitempty"`
	Choices []string `json:"choices"`
	// Withheld marks a node the document itself asked to leave unbound, rather
	// than one FloMorphic held back on finding the action ambiguous.
	Withheld bool `json:"withheld,omitempty"`
}

// ImportProblem is a design problem the planner reported (error = the node or
// edge was dropped, warn = kept as-is).
type ImportProblem struct {
	Level   string `json:"level"`
	At      string `json:"at,omitempty"`
	Message string `json:"message"`
}

// ImportResult is what POST /flow/import answers.
type ImportResult struct {
	OK             bool            `json:"ok"`
	Flow           Flow            `json:"flow"`
	Problems       []ImportProblem `json:"problems"`
	MissingActions []MissingAction `json:"missingActions"`
	UnboundNodes   []UnboundNode   `json:"unboundNodes"`
	CompileError   string          `json:"compileError"`
	DryRun         bool            `json:"dryRun"`
}

// ImportFlow lands a workflow export document as a flow. `id` overwrites an
// existing flow (a re-install); empty creates one. `title` overrides the
// file's own. FloMorphic re-stamps every plugin node from its own extension
// table, so the file may come from any install.
//
// `tags` are the labels to stamp on the flow — the operation it belongs to, so
// the run picker can narrow to it. Empty leaves whatever labels the document
// itself carries, which keeps a package that declares none working exactly as
// it did before tags existed.
func (c *Client) ImportFlow(ctx context.Context, id, title string, tags []string, workflow json.RawMessage, dryRun bool) (*ImportResult, error) {
	body := map[string]any{"workflow": workflow, "dryRun": dryRun}
	if id = strings.TrimSpace(id); id != "" {
		body["id"] = id
	}
	if title = strings.TrimSpace(title); title != "" {
		body["title"] = title
	}
	if clean := tagutil.Clean(tags); len(clean) > 0 {
		body["tags"] = clean
	}
	data, err := c.do(ctx, http.MethodPost, "/flow/import", body)
	if err != nil {
		return nil, err
	}
	var res ImportResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("invalid import result: %w", err)
	}
	if res.Problems == nil {
		res.Problems = []ImportProblem{}
	}
	if res.MissingActions == nil {
		res.MissingActions = []MissingAction{}
	}
	if res.UnboundNodes == nil {
		res.UnboundNodes = []UnboundNode{}
	}
	return &res, nil
}

// TagFlow relabels a flow already on FloMorphic (PUT /flow/id/:id/tags). When
// merge is set the labels are added to what the flow carries; otherwise they
// replace them.
//
// This is the "link to an existing flow" path's other half: an operator who
// imported an operation's flow in FloMorphic by hand and then tells Venapce
// which flow it became has a flow with no labels on it, which the operation
// filter would never find. Relabelling it is a 60-byte call — importing the
// graph again just to add a label would push tens of kilobytes and could
// overwrite edits the operator made.
func (c *Client) TagFlow(ctx context.Context, id string, tags []string, merge bool) ([]string, error) {
	mode := "set"
	if merge {
		mode = "merge"
	}
	body := map[string]any{"tags": tagutil.Clean(tags), "mode": mode}
	data, err := c.do(ctx, http.MethodPut, "/flow/id/"+url.PathEscape(strings.TrimSpace(id))+"/tags", body)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("invalid tag result: %w", err)
	}
	return res.Tags, nil
}

// PluginAction is one action method a registered plugin exposes, as the
// extension table holds it after a sync.
type PluginAction struct {
	Action     string `json:"action"`
	PluginID   string `json:"pluginId"`
	PluginName string `json:"pluginName"`
}

// ListPluginActions returns every action the install's synced plugins expose —
// the portable key an operation's requirements are matched against (a plugin
// id is a per-install address; an action method name is the same everywhere).
func (c *Client) ListPluginActions(ctx context.Context) ([]PluginAction, error) {
	data, err := c.do(ctx, http.MethodGet, "/extension?kind=extension&per_page=500", nil)
	if err != nil {
		return nil, err
	}
	var p page[struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		PluginID string `json:"pluginId"`
		Action   string `json:"action"`
	}]
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid extension list: %w", err)
	}
	names := map[string]string{}
	for _, row := range p.List {
		if row.Action == "" && row.PluginID != "" {
			names[row.PluginID] = row.Name
		}
	}
	out := []PluginAction{}
	for _, row := range p.List {
		if row.Action == "" || row.PluginID == "" {
			continue
		}
		name := names[row.PluginID]
		if name == "" {
			name = row.PluginID
		}
		out = append(out, PluginAction{Action: row.Action, PluginID: row.PluginID, PluginName: name})
	}
	return out, nil
}

// ExportFlow reads a saved flow back as the portable workflow document (GET
// /flow/id/:id/export) — plugin nodes carry only their action, never this
// install's ids, so the document can be packaged and imported anywhere.
func (c *Client) ExportFlow(ctx context.Context, id string) (json.RawMessage, error) {
	data, err := c.do(ctx, http.MethodGet, "/flow/id/"+strings.TrimSpace(id)+"/export", nil)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || data[0] != '{' {
		return nil, fmt.Errorf("invalid export document")
	}
	return data, nil
}

// SettingsProfile is one node settings profile on the install: the named
// key/value bundle (an account, an API key, a provider endpoint) a node binds
// by id. Venapce never reads a profile's values — only that it exists and what
// it is called, so it can tell the operator which nodes still have nothing to
// bind.
type SettingsProfile struct {
	ID         string `json:"id"`
	NodeUniqID string `json:"nodeUniqId"`
	NodeType   string `json:"nodeType"`
	Title      string `json:"title"`
}

// SettingsBucket is the identity a profile binds to, as FloMorphic keys them:
// a node of an imported plugin gets its own bucket per plugin identity, and
// every builtin shares one bucket per node kind (all `llm` nodes offer the same
// profiles). pluginID is this install's address for the plugin, so the bucket
// is resolved from the action list, never carried in a package.
func SettingsBucket(kind, pluginID string) string {
	if p := strings.TrimSpace(pluginID); p != "" {
		return "ext:" + p
	}
	return strings.TrimSpace(kind)
}

// ListSettingsProfiles returns the profiles in one bucket (see SettingsBucket)
// — what the node's drawer offers in its profile picker. An empty answer means
// the operator has to create one before any node of that kind can run.
func (c *Client) ListSettingsProfiles(ctx context.Context, bucket string) ([]SettingsProfile, error) {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil, fmt.Errorf("a settings bucket is required")
	}
	q := url.Values{"node": {bucket}, "per_page": {"200"}}
	data, err := c.do(ctx, http.MethodGet, "/settings?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var p page[SettingsProfile]
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid settings list: %w", err)
	}
	out := make([]SettingsProfile, 0, len(p.List))
	for _, prof := range p.List {
		if strings.TrimSpace(prof.ID) != "" {
			out = append(out, prof)
		}
	}
	return out, nil
}
