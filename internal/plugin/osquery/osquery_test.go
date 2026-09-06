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
