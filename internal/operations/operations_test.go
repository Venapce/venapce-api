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
