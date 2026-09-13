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
