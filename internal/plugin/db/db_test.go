package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildForms is exercised for every table so a malformed form (which panics in
// formkit.Build) is caught here rather than as a dialog that will not open.
func TestFormsParse(t *testing.T) {
	for _, tbl := range []table{issues, stages} {
		up, upd := buildForms(tbl)
		for _, f := range []struct {
			name           string
			schema, uiJSON string
		}{
			{tbl.name + " upsert", up.Jsonschema, up.Jsonui},
			{tbl.name + " update", upd.Jsonschema, upd.Jsonui},
		} {
			var schema map[string]any
			if err := json.Unmarshal([]byte(f.schema), &schema); err != nil {
				t.Fatalf("%s: schema is not valid JSON: %v", f.name, err)
			}
			var ui map[string]any
			if err := json.Unmarshal([]byte(f.uiJSON), &ui); err != nil {
				t.Fatalf("%s: uischema is not valid JSON: %v", f.name, err)
			}
			// The connections are venapce's, never per-node: no action form may
			// declare a settings field.
			if props, ok := schema["properties"].(map[string]any); ok {
				if _, bad := props["settings"]; bad {
					t.Errorf("%s: form must not declare a `settings` property", f.name)
				}
			}
		}
	}
}

// The writable columns are derived from the sqlc model, not hand-listed, so a
// schema change flows through. This pins the derivation: the two data tables must
// expose exactly their non-managed columns, with id and the timestamps excluded.
func TestColumnsDerivedFromModel(t *testing.T) {
	got := func(tbl table) []string {
		out := make([]string, len(tbl.columns))
		for i, c := range tbl.columns {
			out[i] = c.name
		}
		return out
	}
	wantIssues := []string{"title", "summary", "status", "severity", "tags", "source", "assignee", "data"}
	if g := got(issues); strings.Join(g, ",") != strings.Join(wantIssues, ",") {
		t.Errorf("issues columns = %v, want %v", g, wantIssues)
	}
	wantStages := []string{"title", "summary", "source", "disposition", "issue_id", "tags", "data"}
	if g := got(stages); strings.Join(g, ",") != strings.Join(wantStages, ",") {
		t.Errorf("stage columns = %v, want %v", g, wantStages)
	}
	if !issues.hasUpdatedAt || !stages.hasUpdatedAt {
		t.Error("both tables have updated_at and should stamp it")
	}
	for _, tbl := range []table{issues, stages} {
		for _, c := range tbl.columns {
			if c.name == "id" || strings.HasSuffix(c.name, "_at") {
				t.Errorf("%s: managed column %q must not be writable", tbl.name, c.name)
			}
		}
	}
}

func TestCamelToSnake(t *testing.T) {
	cases := map[string]string{"id": "id", "issueId": "issue_id", "createdAt": "created_at", "queryContext": "query_context"}
	for in, want := range cases {
		if got := camelToSnake(in); got != want {
			t.Errorf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildUpsertWithID(t *testing.T) {
	sql, args, err := buildUpsert(issues, map[string]any{
		"id":    float64(5),
		"title": "boom",
		"tags":  []any{"net"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"INSERT INTO issues (id, title, tags) VALUES ($1, $2, $3)",
		"ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, tags = EXCLUDED.tags, updated_at = now()",
		"RETURNING *",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql missing %q\n got: %s", want, sql)
		}
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args, got %d: %v", len(args), args)
	}
	if args[0].(int64) != 5 || args[1].(string) != "boom" {
		t.Errorf("unexpected args: %v", args)
	}
	if tags, ok := args[2].([]string); !ok || len(tags) != 1 || tags[0] != "net" {
		t.Errorf("tags not bound as []string: %#v", args[2])
	}
}

func TestBuildUpsertNoIDIsPlainInsert(t *testing.T) {
	sql, _, err := buildUpsert(issues, map[string]any{"title": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "ON CONFLICT") {
		t.Errorf("insert without id must not have ON CONFLICT: %s", sql)
	}
	if !strings.HasPrefix(sql, "INSERT INTO issues (title) VALUES ($1)") {
		t.Errorf("unexpected sql: %s", sql)
	}
}

func TestBuildUpdateByID(t *testing.T) {
	sql, args, err := buildUpdate(stages, map[string]any{
		"id":          float64(3),
		"disposition": "promoted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sql != "UPDATE stage SET disposition = $2, updated_at = now() WHERE id = $1 RETURNING *" {
		t.Errorf("unexpected sql: %s", sql)
	}
	if len(args) != 2 || args[0].(int64) != 3 || args[1].(string) != "promoted" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestBuildUpdateRequiresID(t *testing.T) {
	if _, _, err := buildUpdate(issues, map[string]any{"title": "x"}); err == nil {
		t.Error("update without id must fail")
	}
}

func TestBuildUpdateNeedsAField(t *testing.T) {
	if _, _, err := buildUpdate(issues, map[string]any{"id": float64(1)}); err == nil {
		t.Error("update with only id must fail")
	}
}

func TestJSONColumnValidation(t *testing.T) {
	if _, _, err := buildUpsert(issues, map[string]any{"data": "{not json"}); err == nil {
		t.Error("invalid JSON in a json column must fail")
	}
	sql, args, err := buildUpsert(issues, map[string]any{"data": `{"k":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "INSERT INTO issues (data)") {
		t.Errorf("unexpected sql: %s", sql)
	}
	if _, ok := args[0].([]byte); !ok {
		t.Errorf("json column not bound as bytes: %#v", args[0])
	}
}
