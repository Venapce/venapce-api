package operations

import (
	"encoding/json"
	"strings"
	"testing"
)

// manifestJSON is the smallest manifest that parses, with the flow-tag fields
// under test spliced in.
func manifestJSON(t *testing.T, flowTags string, flows string) *Manifest {
	t.Helper()
	raw := `{
	  "schema": 1, "id": "gdpr-core", "name": "GDPR core", "version": "1.0.0",
	  "tags": ["demo", "compliance"]` + flowTags + `,
	  "flows": [` + flows + `]
	}`
	m, errs := ParseManifest([]byte(raw))
	if m == nil || len(errs) > 0 {
		t.Fatalf("manifest errors: %v", errs)
	}
	return m
}

const sweepFlow = `{"key":"sweep","file":"flows/sweep.flow.json","role":"entry"}`

// A package that says nothing about flow labels still has to be filterable:
// the run picker's operation filter is useless if a operation's flows land on
// the canvas unlabelled.
func TestFlowTagsDefaultToTheFrameworkID(t *testing.T) {
	m := manifestJSON(t, "", sweepFlow)
	if got := strings.Join(m.FlowTagsFor("sweep"), "|"); got != "operation:gdpr-core" {
		t.Fatalf("FlowTagsFor = %q, want the implicit operation label", got)
	}
	if got := strings.Join(m.AllFlowTags(), "|"); got != "operation:gdpr-core" {
		t.Fatalf("AllFlowTags = %q", got)
	}
	// The package's own `tags` classify the package for a catalog and must not
	// leak onto the flows — "demo" has no business in the picker's filter.
	for _, l := range m.AllFlowTags() {
		if l == "demo" || l == "compliance" {
			t.Fatalf("catalog tag %q leaked into the flow labels: %v", l, m.AllFlowTags())
		}
	}
}

func TestFlowTagsUnionPackageAndFlow(t *testing.T) {
	m := manifestJSON(t, `, "flowTags": ["GDPR", "operation:gdpr-core"]`,
		sweepFlow+`,{"key":"review","file":"flows/review.flow.json","role":"on-row","subject":["finding"],"tags":["on row","GDPR"]}`)

	// The package's labels apply to every flow, normalized (lower-cased).
	if got := strings.Join(m.FlowTagsFor("sweep"), "|"); got != "gdpr|operation:gdpr-core" {
		t.Fatalf("sweep labels = %q", got)
	}
	// A flow's own labels are added, de-duplicated against the package's.
	if got := strings.Join(m.FlowTagsFor("review"), "|"); got != "gdpr|operation:gdpr-core|on-row" {
		t.Fatalf("review labels = %q", got)
	}
	// The operation filter offers the union of everything it stamps.
	if got := strings.Join(m.AllFlowTags(), "|"); got != "gdpr|operation:gdpr-core|on-row" {
		t.Fatalf("AllFlowTags = %q", got)
	}
	// An unknown key is not a panic and not a silent empty set: it still gets
	// the package's labels, because that is what an install of it would stamp.
	if got := strings.Join(m.FlowTagsFor("nope"), "|"); got != "gdpr|operation:gdpr-core" {
		t.Fatalf("unknown key = %q", got)
	}
}

// Packing is the authoring side of the same contract: what the dialog sends
// has to come back out of the manifest it writes.
func TestPackWritesFlowTags(t *testing.T) {
	export := json.RawMessage(`{"flomorphic":{"kind":"workflow","version":1},"title":"Sweep","nodes":[{"ref":"a","kind":"startNode"}]}`)
	in := PackInput{
		Name: "GDPR core", FlowTags: []string{"GDPR"},
		Flows: []PackFlow{{FlowID: "flow_a", Key: "sweep", Role: "entry", Tags: []string{"Nightly Sweep"}}},
	}
	out, _, errs := Pack(in, map[string]json.RawMessage{"flow_a": export})
	if len(errs) > 0 {
		t.Fatalf("pack: %v", errs)
	}
	if got := strings.Join(out.Manifest.FlowTags, "|"); got != "gdpr" {
		t.Fatalf("manifest flowTags = %q", got)
	}
	if got := strings.Join(out.Manifest.Flows[0].Tags, "|"); got != "nightly-sweep" {
		t.Fatalf("flow tags = %q", got)
	}
	// The README names the labels, since they are what the operator types into
	// the picker's filter to find the flow again.
	if readme := out.Files["README.md"]; !strings.Contains(readme, "labelled `gdpr`, `nightly-sweep`") {
		t.Fatalf("README does not name the labels:\n%s", readme)
	}

	// No flowTags given: the id becomes the label, and it is written down
	// rather than left implicit — the manifest is what a human reads.
	bare, _, errs := Pack(PackInput{
		Name:  "GDPR core",
		Flows: []PackFlow{{FlowID: "flow_a", Key: "sweep", Role: "entry"}},
	}, map[string]json.RawMessage{"flow_a": export})
	if len(errs) > 0 {
		t.Fatalf("pack bare: %v", errs)
	}
	if got := strings.Join(bare.Manifest.FlowTags, "|"); got != "operation:gdpr-core" {
		t.Fatalf("default flowTags = %q", got)
	}
}
