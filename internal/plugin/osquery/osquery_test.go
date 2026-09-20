package osquery

import (
	"encoding/json"
	"testing"
)

func TestQueryFormParsesAndHasNoSettings(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(queryForm.Jsonschema), &schema); err != nil {
		t.Fatalf("query form schema is not valid JSON: %v", err)
	}
	var ui map[string]any
	if err := json.Unmarshal([]byte(queryForm.Jsonui), &ui); err != nil {
		t.Fatalf("query form uischema is not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, bad := props["settings"]; bad {
		t.Error("query form must not declare a `settings` property")
	}
	for _, want := range []string{"env", "uuid", "sql"} {
		if _, ok := props[want]; !ok {
			t.Errorf("query form missing property %q", want)
		}
	}
}

func TestNodeOptions(t *testing.T) {
	raw := json.RawMessage(`[
		{"uuid":"u1","localname":"web-1","hostname":"web-1.local","platform":"ubuntu"},
		{"uuid":"u2","hostname":"db-1","platform":"darwin"},
		{"hostname":"no-uuid-skipped"}
	]`)
	opts := nodeOptions(raw)
	if len(opts) != 2 {
		t.Fatalf("want 2 options (uuid-less skipped), got %d: %#v", len(opts), opts)
	}
	if opts[0].Value != "u1" || opts[0].Label != "web-1 · ubuntu" {
		t.Errorf("unexpected first option: %#v", opts[0])
	}
	if opts[1].Value != "u2" || opts[1].Label != "db-1 · darwin" {
		t.Errorf("unexpected second option: %#v", opts[1])
	}
}

func TestEnvOptions(t *testing.T) {
	raw := json.RawMessage(`[{"name":"corp","hostname":"osctrl.corp"},{"uuid":"x"}]`)
	opts := envOptions(raw)
	if len(opts) != 1 {
		t.Fatalf("want 1 option (nameless skipped), got %d", len(opts))
	}
	if opts[0].Value != "corp" || opts[0].Label != "corp · osctrl.corp" {
		t.Errorf("unexpected option: %#v", opts[0])
	}
}

func TestColumnsOfUnion(t *testing.T) {
	rows := []map[string]any{
		{"a": 1, "b": 2},
		{"a": 3, "c": 4},
	}
	cols := columnsOf(rows)
	if len(cols) != 3 {
		t.Fatalf("want 3 columns, got %d: %v", len(cols), cols)
	}
	seen := map[string]bool{}
	for _, c := range cols {
		seen[c] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !seen[want] {
			t.Errorf("missing column %q in %v", want, cols)
		}
	}
}

func TestQueryByTagsFormDeclaresArrayTags(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(queryByTagsForm.Jsonschema), &schema); err != nil {
		t.Fatalf("queryByTags form schema is not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	for _, want := range []string{"env", "tags", "sql"} {
		if _, ok := props[want]; !ok {
			t.Errorf("queryByTags form missing property %q", want)
		}
	}
	tags, _ := props["tags"].(map[string]any)
	if tags["type"] != "array" {
		t.Errorf("tags must be an array property, got %v", tags["type"])
	}
	if tags["uniqueItems"] != true {
		t.Errorf("tags must set uniqueItems so the renderer draws a multi-select")
	}
	req, _ := schema["required"].([]any)
	found := false
	for _, r := range req {
		if r == "tags" {
			found = true
		}
	}
	if !found {
		t.Errorf("tags must be required, got required=%v", req)
	}
}

func TestTagOptions(t *testing.T) {
	raw := json.RawMessage(`[
		{"name":"linux","description":"linux","auto_tag":true,"tag_type":2},
		{"name":"web","description":"front-end fleet","auto_tag":false},
		{"description":"nameless-skipped"}
	]`)
	opts := tagOptions(raw)
	if len(opts) != 2 {
		t.Fatalf("want 2 options (nameless skipped), got %d: %#v", len(opts), opts)
	}
	if opts[0].Value != "linux" || opts[0].Label != "linux (auto)" {
		t.Errorf("unexpected first option: %#v", opts[0])
	}
	if opts[1].Value != "web" || opts[1].Label != "web · front-end fleet" {
		t.Errorf("unexpected second option: %#v", opts[1])
	}
}

func TestCleanTagsAndUnknownTags(t *testing.T) {
	got := cleanTags([]string{" linux ", "", "web", "linux"})
	if len(got) != 2 || got[0] != "linux" || got[1] != "web" {
		t.Fatalf("cleanTags: want [linux web], got %v", got)
	}
	known := tagOptions(json.RawMessage(`[{"name":"linux"},{"name":"web"}]`))
	if missing := unknownTags([]string{"linux", "typo", "web", "other"}, known); len(missing) != 2 || missing[0] != "typo" || missing[1] != "other" {
		t.Errorf("unknownTags: want [typo other], got %v", missing)
	}
	if missing := unknownTags([]string{"linux"}, known); len(missing) != 0 {
		t.Errorf("unknownTags: want none, got %v", missing)
	}
}

func TestEnvironmentButtonNamesItsOwnForm(t *testing.T) {
	// The Environments lookup is shared by both forms; each button must say
	// which form to rebuild, or the picker swaps the dialog to the other action.
	for _, tc := range []struct {
		form   string
		ui     string
		method string
	}{
		{"query", queryForm.Jsonui, methodQuery},
		{"queryByTags", queryByTagsForm.Jsonui, methodQueryByTags},
	} {
		var ui map[string]any
		if err := json.Unmarshal([]byte(tc.ui), &ui); err != nil {
			t.Fatalf("%s uischema: %v", tc.form, err)
		}
		var found bool
		var walk func(any)
		walk = func(n any) {
			m, ok := n.(map[string]any)
			if !ok {
				if arr, ok := n.([]any); ok {
					for _, e := range arr {
						walk(e)
					}
				}
				return
			}
			if m["scope"] == "#/properties/env" {
				found = true
				x, _ := m["x-inflow-ui"].(map[string]any)
				action, _ := x["action"].(map[string]any)
				body, _ := action["body"].(map[string]any)
				if body["form"] != tc.method {
					t.Errorf("%s: env button rebuilds %v, want %s", tc.form, body["form"], tc.method)
				}
			}
			for _, v := range m {
				walk(v)
			}
		}
		walk(ui)
		if !found {
			t.Errorf("%s: no env control in uischema", tc.form)
		}
	}
	if formFor(methodQueryByTags).Jsonschema != queryByTagsForm.Jsonschema || formFor("").Jsonschema != queryForm.Jsonschema {
		t.Error("formFor must map the method to its own form and default to the node form")
	}
}

// A failed execution comes back from osctrl as a result row with status != 0
// and osquery's message inside the data envelope; it must leave `rows` and
// surface as a failure the flow context can read.
func TestSplitFailuresPullsErroredExecutionsOutOfRows(t *testing.T) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(`[
		{"uuid":"NODE-A","name":"q","status":0,"data":"{\"name\":\"q\",\"result\":[{\"pid\":\"1\"}],\"status\":0,\"message\":\"\"}"},
		{"uuid":"NODE-B","name":"q","status":1,"data":"{\"name\":\"q\",\"result\":[],\"status\":1,\"message\":\"no such table: prcesses\"}"},
		{"uuid":"NODE-C","name":"q","data":"{\"name\":\"q\",\"result\":[],\"status\":2,\"message\":\"no such column: x\"}"}
	]`), &rows); err != nil {
		t.Fatal(err)
	}
	decodeRowData(rows)
	ok, failures := splitFailures(rows)

	if len(ok) != 1 || ok[0]["uuid"] != "NODE-A" {
		t.Fatalf("want only NODE-A as data, got %v", ok)
	}
	if len(failures) != 2 {
		t.Fatalf("want 2 failures, got %v", failures)
	}
	if failures[0]["uuid"] != "NODE-B" || failures[0]["status"] != 1 || failures[0]["message"] != "no such table: prcesses" {
		t.Errorf("failure from top-level status wrong: %v", failures[0])
	}
	if failures[1]["uuid"] != "NODE-C" || failures[1]["status"] != 2 || failures[1]["message"] != "no such column: x" {
		t.Errorf("failure from envelope status wrong: %v", failures[1])
	}

	if s := failureSummary(2, failures); s != "osquery failed on 2 node(s): no such table: prcesses; no such column: x" {
		t.Errorf("unexpected summary %q", s)
	}
	if s := failureSummary(1, nil); s != "osquery failed on 1 node(s): osctrl recorded no error message; check the query in the osctrl panel" {
		t.Errorf("unexpected message-less summary %q", s)
	}
}

func TestSplitFailuresKeepsRowsNonNil(t *testing.T) {
	ok, failures := splitFailures([]map[string]any{{"uuid": "N", "status": float64(1), "data": map[string]any{"message": "boom"}}})
	if ok == nil || len(ok) != 0 {
		t.Errorf("rows must be an empty slice, not nil: %#v", ok)
	}
	if len(failures) != 1 || failures[0]["message"] != "boom" {
		t.Errorf("unexpected failures %v", failures)
	}
}

// A node that ran the SQL and matched nothing still yields one envelope with an
// empty result, so the osquery row count must come from inside the envelopes.
func TestResultRowCountLooksInsideEnvelopes(t *testing.T) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(`[
		{"uuid":"A","status":0,"data":"{\"result\":[],\"status\":0}"},
		{"uuid":"B","status":0,"data":"{\"result\":[{\"pid\":\"1\"},{\"pid\":\"2\"}],\"status\":0}"},
		{"uuid":"C","status":0,"data":"not json"}
	]`), &rows); err != nil {
		t.Fatal(err)
	}
	decodeRowData(rows)
	if n := resultRowCount(rows); n != 2 {
		t.Errorf("want 2 osquery rows across envelopes, got %d", n)
	}
	if n := resultRowCount(rows[:1]); n != 0 {
		t.Errorf("empty result must count 0, got %d", n)
	}
}
