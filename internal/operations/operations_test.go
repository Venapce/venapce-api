package operations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The example package shipped with the wapp (the cookbook fleet audit,
// parameterized) is the fixture: what the folder importer sends.
const examplePkg = "../../../venapce-wapp/examples/operations/linux-fleet-http-audit"

func loadExample(t *testing.T) *Bundle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(examplePkg, ManifestFile))
	if err != nil {
		t.Skipf("example package not checked out next to this repo: %v", err)
	}
	m, errs := ParseManifest(raw)
	if m == nil || len(errs) > 0 {
		t.Fatalf("manifest errors: %v", errs)
	}
	files := map[string]string{}
	for _, p := range m.Files() {
		b, err := os.ReadFile(filepath.Join(examplePkg, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		files[p] = string(b)
	}
	return &Bundle{Manifest: m, Files: files}
}

func TestExamplePackageChecksClean(t *testing.T) {
	b := loadExample(t)
	if errs := b.Check(); len(errs) > 0 {
		t.Fatalf("check: %v", errs)
	}
	ref := b.ReferencedActions()
	want := []string{"db.stages.upsert", "osquery.query", "osquery.queryByTags"}
	if strings.Join(ref["http-audit"], ",") != strings.Join(want, ",") {
		t.Fatalf("referenced actions = %v, want %v", ref["http-audit"], want)
	}
	if b.Manifest.Flows[0].Step != 1 || b.Manifest.Flows[0].Doc == "" {
		t.Fatalf("step/doc not read: %+v", b.Manifest.Flows[0])
	}
}

func TestReadmeIsRequired(t *testing.T) {
	b := loadExample(t)
	delete(b.Files, "README.md")
	errs := b.Check()
	if len(errs) != 1 || !strings.Contains(errs[0], "README") {
		t.Fatalf("expected the README error, got %v", errs)
	}
}

func TestSubstituteKeepsParamTypes(t *testing.T) {
	b := loadExample(t)
	values := map[string]json.RawMessage{"fleetTags": json.RawMessage(`["prod","web"]`)}
	eff := Effective(b.Manifest, values)
	if string(eff["env"]) != `"default"` {
		t.Fatalf("default not applied: %s", eff["env"])
	}
	out, err := Substitute([]byte(b.Files["flows/http-audit.flow.json"]), eff)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []struct {
			Ref  string `json:"ref"`
			Data struct {
				Body struct {
					Env  string   `json:"env"`
					Tags []string `json:"tags"`
				} `json:"body"`
			} `json:"data"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, n := range doc.Nodes {
		if n.Ref == "fleet-tcp-listeners-owning-proce" {
			if n.Data.Body.Env != "default" || strings.Join(n.Data.Body.Tags, ",") != "prod,web" {
				t.Fatalf("substitution wrong: %+v", n.Data.Body)
			}
			return
		}
	}
	t.Fatal("sweep node not found")
}

func TestMissingParams(t *testing.T) {
	m := &Manifest{Params: map[string]ParamSpec{
		"repo":   {Type: "string", Required: true},
		"labels": {Type: "string[]", Default: json.RawMessage(`["x"]`), Required: true},
	}}
	if got := Missing(m, nil); len(got) != 1 || got[0] != "repo" {
		t.Fatalf("missing = %v", got)
	}
	if got := Missing(m, map[string]json.RawMessage{"repo": json.RawMessage(`"o/r"`)}); len(got) != 0 {
		t.Fatalf("missing = %v", got)
	}
}

func TestResolveURL(t *testing.T) {
	cases := map[string]struct{ base, path, ref, catalog string }{
		"https://github.com/FloMorphic/flow-cookbook/tree/main/linux-fleet-http-audit": {
			"https://raw.githubusercontent.com/FloMorphic/flow-cookbook/main/linux-fleet-http-audit/", "linux-fleet-http-audit", "main", ""},
		"https://github.com/venapce/operations": {
			"https://raw.githubusercontent.com/venapce/operations/main/", "", "main", "https://raw.githubusercontent.com/venapce/operations/main/catalog.json"},
		"https://github.com/o/r/blob/v2/ops/x/operation.json": {
			"https://raw.githubusercontent.com/o/r/v2/ops/x/", "ops/x", "v2", ""},
		"https://raw.githubusercontent.com/o/r/dev/pkg/operation.json": {
			"https://raw.githubusercontent.com/o/r/dev/pkg/", "pkg", "dev", ""},
		"https://example.com/ops/audit/": {"https://example.com/ops/audit/", "", "", ""},
	}
	for in, want := range cases {
		r, err := ResolveURL(in, "main")
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if r.RawBase != want.base || r.Source.Path != want.path || r.Source.Ref != want.ref || r.CatalogAt != want.catalog {
			t.Fatalf("%s → %+v (base %s), want %+v", in, r.Source, r.RawBase, want)
		}
	}
	if _, err := ResolveURL("not a url", "main"); err == nil {
		t.Fatal("expected an error for garbage")
	}
}

func TestPackFromExports(t *testing.T) {
	b := loadExample(t)
	export := json.RawMessage(b.Files["flows/http-audit.flow.json"])
	in := PackInput{
		Name: "Fleet HTTP audit (packed)", Description: "Who serves HTTP on the fleet.",
		Tags: []string{"linux"}, Scale: []string{"fleet"},
		Params: map[string]ParamSpec{"env": {Type: "string", Default: json.RawMessage(`"default"`), Required: true}},
		Flows: []PackFlow{
			{FlowID: "flow_a", Role: "entry", Schedule: "0 3 * * *", Writes: []string{"stage"}},
			{FlowID: "flow_b", Key: "triage", Role: "on-row", Subject: []string{"stage"}, Title: "Triage a row"},
		},
	}
	out, keyOf, errs := Pack(in, map[string]json.RawMessage{"flow_a": export, "flow_b": export})
	if len(errs) > 0 {
		t.Fatalf("pack: %v", errs)
	}
	m := out.Manifest
	if m.ID != "fleet-http-audit-packed" || m.Version != "1.0.0" {
		t.Fatalf("id/version derived wrong: %s %s", m.ID, m.Version)
	}
	if keyOf["flow_b"] != "triage" || keyOf["flow_a"] != "linux-fleet-http-nginx-served-hostnames-audit" {
		t.Fatalf("keys: %v", keyOf)
	}
	if m.Flows[0].Step != 1 || m.Flows[1].Step != 2 || m.Flows[0].File != "flows/"+keyOf["flow_a"]+".flow.json" {
		t.Fatalf("flows: %+v", m.Flows)
	}
	if m.Requires == nil || m.Requires.Osctrl == nil || len(m.Requires.Plugins) != 1 || m.Requires.Plugins[0].Name != "venapce" {
		t.Fatalf("requires not derived from the export: %+v", m.Requires)
	}
	if strings.Join(m.Requires.Plugins[0].Actions, ",") != "db.stages.upsert,osquery.query,osquery.queryByTags" {
		t.Fatalf("actions: %v", m.Requires.Plugins[0].Actions)
	}
	readme := out.Files["README.md"]
	for _, want := range []string{"# Fleet HTTP audit (packed)", "## Mission", "### Step 1 —", "### Step 2 — Triage a row (`triage`, on a stage)", "**venapce** plugin", "- `env` —"} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README scaffold lacks %q:\n%s", want, readme)
		}
	}
	if errs := out.Check(); len(errs) > 0 {
		t.Fatalf("packed bundle does not pass Check: %v", errs)
	}
	z, err := Zip(out)
	if err != nil || len(z) < 100 {
		t.Fatalf("zip: %v (%d bytes)", err, len(z))
	}
	if _, ok := out.Files[ManifestFile]; ok {
		t.Fatal("manifest must not be stored in files")
	}
}
