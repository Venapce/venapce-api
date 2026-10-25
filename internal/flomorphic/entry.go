package flomorphic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A flow's entry form — the input a flow declares it needs before it runs.
//
// FloMorphic lets a flow designer declare that input once (`preferences.entry`)
// instead of wiring a JS node after the start node whose only job was to put a
// fixed shape into the context. The declaration is a form any launcher can
// render, and the engine validates what comes back against the flow's own
// schema and seeds it into the one context location the entry names.
//
// venapce is one of those launchers: a row's "Run flow" dialog renders the
// form, and the filled values travel with the launch (POST /process `entry`).
// Nothing here resolves or validates the form itself — that would be two
// implementations of one contract, and the second one would drift. The engine
// is the only thing that writes the seed.

// EntryTargetDefault is where entry values land when a form names no target.
// Mirrors models.EntryTargetDefault on the FloMorphic side; it is here so a
// dialog can say "seeded at $._entry" without a round trip.
const EntryTargetDefault = "$._entry"

// MetaEntryKey is the slot on a FloMorphic process row that holds the entry
// values a run was launched with (inflow.MetaEntryKey). The engine keeps them
// there deliberately: a context document moves on as the run writes to it, so
// the row's meta is the only durable record of what a run was actually GIVEN.
// That makes it the one place to read "what did the last run of this flow use"
// from — not the run's final context, where a later node may have overwritten
// the seed.
const MetaEntryKey = "entry"

// FlowPreferences are a flow's flow-level, non-graph choices. One field today;
// an object so the next preference does not change the shape again.
type FlowPreferences struct {
	Entry *EntryForm `json:"entry,omitempty"`
}

// EntryForm is the form a flow asks to have filled in before it runs.
type EntryForm struct {
	// Title / Description head the form; empty falls back to the flow's title.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Form is the entry document (see EntryDocument).
	Form EntryDocument `json:"form"`
	// Target is the ONE context location the submitted values are written to —
	// `$._entry` by default. Nothing else in the context is touched.
	Target string `json:"target,omitempty"`
	// Prefill is what a launcher starts the fields at: "default" (the schema's
	// own defaults) or "live" (whatever already sits at Target in the context
	// the run goes against). venapce builds a fresh context document per run,
	// so "live" has nothing to read and resolves to the defaults.
	Prefill string `json:"prefill,omitempty"`
	// Required refuses a launch that supplies no values and finds nothing at
	// the target already — a flow that cannot do anything without its input.
	Required bool `json:"required,omitempty"`
}

// EntryDocument is the entry's form document, written in either of two ways
// (IsSchema tells them apart):
//
//   - a JSON Schema — every control, default, enum and validation rule the
//     renderer supports;
//   - a plain JSON object of the values to seed — the easy way in, rendered as
//     a flat key/value table and seeded as written.
//
// UI is an optional JSON Forms UI schema for the first.
type EntryDocument struct {
	Schema map[string]any `json:"schema,omitempty"`
	UI     map[string]any `json:"ui,omitempty"`
}

// EntryView is GET /flow/id/:id/entry — the entry form resolved for one launch.
// `Entry` is nil for a flow that declares none, which is a launcher's cue to run
// it the way it always did rather than an error.
type EntryView struct {
	Entry   *EntryForm     `json:"entry"`
	Target  string         `json:"target"`
	Prefill string         `json:"prefill"`
	Values  map[string]any `json:"values"`
	// Source says where Values came from: "default" (the schema alone),
	// "context" (the live context) or "none" (the flow declares no entry).
	Source string `json:"source"`
}

// EntryOrNil returns the declared entry form, or nil when the flow asks for
// nothing. Safe on a nil receiver, so a caller can ask
// `flow.Preferences.EntryOrNil()` without first checking for preferences.
func (p *FlowPreferences) EntryOrNil() *EntryForm {
	if p == nil || p.Entry == nil || !p.Entry.HasForm() {
		return nil
	}
	return p.Entry
}

// HasForm reports whether an entry actually describes fields. An empty document
// describes nothing and is treated as "no entry" rather than as a form with no
// inputs.
func (e *EntryForm) HasForm() bool {
	return e != nil && len(e.Form.Schema) > 0
}

// TargetPath is the entry's write path, defaulted.
func (e *EntryForm) TargetPath() string {
	if e == nil {
		return EntryTargetDefault
	}
	if t := strings.TrimSpace(e.Target); t != "" {
		return t
	}
	return EntryTargetDefault
}

// IsSchema reports whether the entry document is a JSON Schema rather than a
// plain object of values.
//
// The test is deliberately narrow — a `$schema` declaration, or a non-empty
// `properties` object — because the alternative is a document whose keys are a
// person's own data, and guessing "schema" from a key like `type` would read
// `{"type": "order"}` as a contract instead of as the value it is. Mirrors
// models.IsSchemaDocument so this side and the engine agree on what a document
// is.
func (e *EntryForm) IsSchema() bool {
	if !e.HasForm() {
		return false
	}
	if _, ok := e.Form.Schema["$schema"]; ok {
		return true
	}
	props, ok := e.Form.Schema["properties"].(map[string]any)
	return ok && len(props) > 0
}

// FlowEntry reads a flow's entry form resolved for a launch
// (GET /flow/id/:id/entry).
//
// No contextId is sent: venapce creates the run's context document at launch
// time, so there is no live context to prefill from and the answer is the
// entry's own defaults. The resolve is left to FloMorphic rather than derived
// here so the dialog's fields start where an agent over MCP and the designer's
// own run button start them.
func (c *Client) FlowEntry(ctx context.Context, flowID string) (EntryView, error) {
	data, err := c.do(ctx, http.MethodGet, "/flow/id/"+url.PathEscape(strings.TrimSpace(flowID))+"/entry", nil)
	if err != nil {
		return EntryView{}, err
	}
	var view EntryView
	if err := json.Unmarshal(data, &view); err != nil {
		return EntryView{}, fmt.Errorf("invalid flow entry: %w", err)
	}
	if view.Target == "" {
		view.Target = EntryTargetDefault
	}
	if view.Values == nil {
		view.Values = map[string]any{}
	}
	if view.Source == "" {
		if view.Entry == nil {
			view.Source = "none"
		} else {
			view.Source = "default"
		}
	}
	return view, nil
}
