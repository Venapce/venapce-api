package flomorphic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The resolved entry form of a flow that declares one. The body is the shape a
// live FloMorphic answers with, `ui: null` included — a plain-values entry
// carries no UI schema and the field is still there.
func TestFlowEntry(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{
			"entry":{"title":"init form","form":{"schema":{"speed":10,"temp":50},"ui":null},
			         "target":"$.input.run","prefill":"live","required":true},
			"target":"$.input.run","prefill":"live","values":{"speed":10,"temp":50},"source":"default"
		},"error":null}`))
	}))
	defer srv.Close()

	view, err := NewClient(srv.URL, "secret").FlowEntry(context.Background(), " flow_1 ")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/flow/id/flow_1/entry" {
		t.Errorf("path = %q", gotPath)
	}
	if view.Entry == nil || view.Entry.Title != "init form" || !view.Entry.Required {
		t.Fatalf("entry = %+v", view.Entry)
	}
	// A plain-values document is not a schema, however many keys it has: those
	// keys are the operator's data, not JSON Schema keywords.
	if view.Entry.IsSchema() {
		t.Error("a plain-values document read as a JSON Schema")
	}
	if view.Entry.TargetPath() != "$.input.run" || view.Target != "$.input.run" {
		t.Errorf("target = %q / %q", view.Entry.TargetPath(), view.Target)
	}
	if view.Values["speed"] != float64(10) || view.Source != "default" {
		t.Errorf("values = %v, source = %q", view.Values, view.Source)
	}
}

// A flow that asks for nothing answers `entry: null` — the launcher's cue to
// run it the way it always did, not an error. The defaults fill in what such a
// body leaves out, so no caller has to special-case an empty view.
func TestFlowEntryNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"entry":null},"error":null}`))
	}))
	defer srv.Close()

	view, err := NewClient(srv.URL, "secret").FlowEntry(context.Background(), "flow_1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Entry != nil {
		t.Fatalf("entry = %+v", view.Entry)
	}
	if view.Target != EntryTargetDefault || view.Source != "none" || view.Values == nil {
		t.Errorf("view = %+v", view)
	}
}

// What makes a document a JSON Schema, and what a flow's declaration reduces to
// when it describes no fields at all.
func TestEntryFormKinds(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		hasForm  bool
		isSchema bool
	}{
		{"declared properties", `{"type":"object","properties":{"threshold":{"type":"string"}}}`, true, true},
		{"a $schema declaration", `{"$schema":"https://json-schema.org/draft/2020-12/schema"}`, true, true},
		{"plain values", `{"speed":10,"temp":50}`, true, false},
		// `{"type":"order"}` is somebody's data. Reading `type` as a keyword
		// would turn their value into a contract.
		{"a value named like a keyword", `{"type":"order"}`, true, false},
		// A schema describing nothing is a form with nothing in it, so it reads
		// as plain and renders as the empty table it is.
		{"an empty object schema", `{"type":"object"}`, true, false},
		{"nothing at all", `{}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e EntryForm
			if err := json.Unmarshal([]byte(`{"form":{"schema":`+tc.schema+`}}`), &e); err != nil {
				t.Fatal(err)
			}
			if e.HasForm() != tc.hasForm || e.IsSchema() != tc.isSchema {
				t.Errorf("hasForm = %v (want %v), isSchema = %v (want %v)", e.HasForm(), tc.hasForm, e.IsSchema(), tc.isSchema)
			}
			prefs := &FlowPreferences{Entry: &e}
			if got := prefs.EntryOrNil() != nil; got != tc.hasForm {
				t.Errorf("EntryOrNil present = %v, want %v", got, tc.hasForm)
			}
		})
	}
	// Defaulted on the nil paths, so a caller can ask without checking first.
	var none *FlowPreferences
	if none.EntryOrNil() != nil {
		t.Error("no preferences must be no entry")
	}
	if (*EntryForm)(nil).TargetPath() != EntryTargetDefault {
		t.Error("the default target is the one a flow that pinned nothing gets")
	}
}

// The entry values travel with the launch, and only when there are any: an
// `entry` key on a flow that declares no form is a seed with nowhere to go.
func TestStartProcessEntry(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = map[string]any{}
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"data":{"id":"proc_1","status":"running"},"error":null}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret")

	if _, err := c.StartProcess(context.Background(), "flow_1", "ctx_1", map[string]any{"venapce": true}, map[string]any{"speed": 12}); err != nil {
		t.Fatal(err)
	}
	entry, ok := body["entry"].(map[string]any)
	if !ok || entry["speed"] != float64(12) {
		t.Fatalf("entry = %v", body["entry"])
	}

	if _, err := c.StartProcess(context.Background(), "flow_1", "ctx_1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, present := body["entry"]; present {
		t.Error("a launch with no entry values must not send an entry key")
	}
}

// What a run was GIVEN survives on the process row, not in its context: the
// engine keeps the submitted values at meta.entry so they outlive a context
// document that the run itself goes on writing to. That is what "the last run
// used these" is read from, so the slot and its shape are a contract.
//
// The body is one real row from a live FloMorphic, trimmed.
func TestProcessMetaEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/process/id/4829" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"indexId":4829,"pid":"proc_x","flowId":"flow_1","contextId":"ctx_1",
			"status":"finished","meta":{"entry":{"speed":10,"temp":344},"startNodes":["n1"]}},"error":null}`))
	}))
	defer srv.Close()

	proc, err := NewClient(srv.URL, "secret").GetProcess(context.Background(), 4829)
	if err != nil {
		t.Fatal(err)
	}
	values, ok := proc.Meta[MetaEntryKey].(map[string]any)
	if !ok {
		t.Fatalf("meta[%q] = %#v, want an object of the values the run was given", MetaEntryKey, proc.Meta[MetaEntryKey])
	}
	if values["temp"] != float64(344) || values["speed"] != float64(10) {
		t.Errorf("values = %v", values)
	}
	// A run launched before entry forms existed, or one given nothing, has no
	// such slot — and that reads as "no offer to make", never as empty values.
	if _, ok := map[string]any{"startNodes": []any{}}[MetaEntryKey].(map[string]any); ok {
		t.Error("an absent slot must not read as values")
	}
}
