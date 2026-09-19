package flomorphic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	CompileError   string          `json:"compileError"`
	DryRun         bool            `json:"dryRun"`
}

// ImportFlow lands a workflow export document as a flow. `id` overwrites an
// existing flow (a re-install); empty creates one. `title` overrides the
// file's own. FloMorphic re-stamps every plugin node from its own extension
// table, so the file may come from any install.
func (c *Client) ImportFlow(ctx context.Context, id, title string, workflow json.RawMessage, dryRun bool) (*ImportResult, error) {
	body := map[string]any{"workflow": workflow, "dryRun": dryRun}
	if id = strings.TrimSpace(id); id != "" {
		body["id"] = id
	}
	if title = strings.TrimSpace(title); title != "" {
		body["title"] = title
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
	return &res, nil
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
