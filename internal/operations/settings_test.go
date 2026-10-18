package operations

import (
	"encoding/json"
	"testing"
)

// A package flow the way an export writes one: plugin nodes carrying only
// their action, a builtin that binds a provider profile (cast), and kinds that
// bind nothing.
const settingsFlow = `{
  "flomorphic": {"kind": "workflow"},
  "title": "Commit review",
  "nodes": [
    {"ref": "start", "kind": "startNode", "title": "Start"},
    {"ref": "clone-the-repository", "kind": "plugin", "title": "Clone the repository", "data": {"action": "github.repo.clone"}},
    {"ref": "review-with-claude", "kind": "plugin", "title": "Review the commits with Claude", "data": {"action": "claude.agent.run", "settingsId": "nset_authors_install"}},
    {"ref": "normalize", "kind": "js", "title": "Normalize", "data": {"logic_rule": "x"}},
    {"ref": "map-each-finding", "kind": "cast", "title": "Map each finding", "data": {"mappings": []}},
    {"ref": "stage-each-finding", "kind": "plugin", "title": "Stage each finding", "data": {"action": "db.stages.upsert"}},
    {"ref": "plain-mcp", "kind": "mcp", "title": "Call a tool", "data": {"mcpMode": "tool"}},
    {"ref": "llm-mcp", "kind": "mcp", "title": "Ask the model", "data": {"mcpMode": "llm"}}
  ]
}`

func TestSettingsNodesIn(t *testing.T) {
	got := SettingsNodesIn("review", []byte(settingsFlow))
	want := []string{"clone-the-repository", "review-with-claude", "map-each-finding", "stage-each-finding", "llm-mcp"}
	if len(got) != len(want) {
		t.Fatalf("found %d profile-bearing nodes, want %d: %+v", len(got), len(want), got)
	}
	for i, ref := range want {
		if got[i].Ref != ref {
			t.Errorf("node %d = %q, want %q", i, got[i].Ref, ref)
		}
		if got[i].FlowKey != "review" {
			t.Errorf("node %d flow key = %q", i, got[i].FlowKey)
		}
	}
	// A plugin node is identified by its action, and the author's own binding
	// rides in the file — the one thing that must be reported, not trusted.
	if got[0].Action != "github.repo.clone" || got[0].Title != "Clone the repository" {
		t.Errorf("first node = %+v", got[0])
	}
	if got[1].SettingsID != "nset_authors_install" {
		t.Errorf("the author's settingsId was not read: %+v", got[1])
	}
	if got[2].Kind != "cast" || got[2].Action != "" {
		t.Errorf("cast node = %+v", got[2])
	}
}

func TestNeedsSettingsProfile(t *testing.T) {
	cases := []struct {
		kind string
		data map[string]any
		want bool
	}{
		{"plugin", nil, true},
		{"llm", nil, true},
		{"ai-decision", nil, true},
		{"cast", nil, true},
		{"http", nil, true},
		{"hitl", nil, true},
		{"mcp", map[string]any{"mcpMode": "llm"}, true},
		{"mcp", map[string]any{"mcpMode": "tool"}, false},
		{"mcp", nil, false},
		{"js", nil, false},
		{"rule", nil, false},
		{"startNode", nil, false},
	}
	for _, c := range cases {
		if got := NeedsSettingsProfile(c.kind, c.data); got != c.want {
			t.Errorf("NeedsSettingsProfile(%q, %v) = %v, want %v", c.kind, c.data, got, c.want)
		}
	}
	if !OptionalSettingsProfile("http") || OptionalSettingsProfile("plugin") {
		t.Error("only an http node runs unbound")
	}
}

// requires.settings[].for is prose an author writes — a list of node titles.
// It has to find the node without the author having to spell its ref.
func TestSettingsReqNames(t *testing.T) {
	nodes := SettingsNodesIn("review", []byte(settingsFlow))
	claude := nodes[1]
	clone := nodes[0]

	if !(SettingsReq{For: "Review the commits with Claude"}).Names(claude) {
		t.Error("the exact title should name the node")
	}
	if !(SettingsReq{For: "Clone the repository, List the recent commits"}).Names(clone) {
		t.Error("a comma-separated list should name each node in it")
	}
	if !(SettingsReq{For: "review-with-claude"}).Names(claude) {
		t.Error("the node's ref should name it")
	}
	if (SettingsReq{For: "Something else"}).Names(claude) {
		t.Error("an unrelated term must not match")
	}
	if (SettingsReq{}).Names(claude) {
		t.Error("an entry with no `for` names nothing")
	}
	// A ref is truncated when it is derived from a long title, so the author's
	// full title is longer than the ref the node kept.
	long := SettingsNode{Ref: "per-node-nginx-config-files-dock", Title: ""}
	if !(SettingsReq{For: "Per-node nginx config files (docker)"}).Names(long) {
		t.Error("a title longer than the truncated ref should still match it")
	}
}

func TestBundleSettingsNodesAndUnmatched(t *testing.T) {
	m, errs := ParseManifest([]byte(`{
      "schema": 1, "id": "pkg", "name": "Pkg", "version": "1.0.0",
      "flows": [{"key": "review", "file": "flows/review.flow.json", "role": "entry"}],
      "requires": {"settings": [
        {"kind": "other", "for": "Review the commits with Claude", "note": "A Claude profile."},
        {"kind": "other", "for": "A node that is not in the package", "note": "Orphan."}
      ]}
    }`))
	if m == nil || len(errs) > 0 {
		t.Fatalf("manifest: %v", errs)
	}
	b := &Bundle{Manifest: m, Files: map[string]string{"flows/review.flow.json": settingsFlow}}

	nodes := b.SettingsNodes()
	if len(nodes) != 5 {
		t.Fatalf("package has %d profile-bearing nodes: %+v", len(nodes), nodes)
	}
	notes := m.SettingsNotesFor(nodes[1])
	if len(notes) != 1 || notes[0].Note != "A Claude profile." {
		t.Fatalf("notes for the Claude node = %+v", notes)
	}
	if len(m.SettingsNotesFor(nodes[0])) != 0 {
		t.Error("the clone node has no declared profile note")
	}
	un := m.UnmatchedSettings(nodes)
	if len(un) != 1 || un[0].Note != "Orphan." {
		t.Fatalf("unmatched = %+v, want only the orphan entry", un)
	}
}

// The document a flow is read back as has the same shape, so one reader serves
// both — that is how a live binding is found.
func TestSettingsNodesInReadsBackLiveBindings(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(settingsFlow), &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["nodes"].([]any)
	nodes[1].(map[string]any)["data"].(map[string]any)["settingsId"] = "nset_live"
	raw, _ := json.Marshal(doc)

	for _, n := range SettingsNodesIn("review", raw) {
		if n.Ref == "clone-the-repository" && n.SettingsID != "nset_live" {
			t.Fatalf("live binding not read: %+v", n)
		}
	}
	if len(SettingsNodesIn("review", []byte("not json"))) != 0 {
		t.Error("an unreadable document yields no nodes")
	}
}

// An install re-derives every node ref from its title and caps the length, so
// the package's refs and the flow's refs are not the same strings. Taken from
// the real pair: the package's "map-each-finding-onto-the-stage-schema" comes
// back as "map-each-finding-onto-the-stage".
func TestMatchSettingsNodesAcrossRederivedRefs(t *testing.T) {
	pkg := []SettingsNode{
		{Ref: "clone-the-repository", Kind: "plugin", Action: "github.repo.clone"},
		{Ref: "list-the-recent-commits", Kind: "plugin", Action: "github.activity.list"},
		{Ref: "map-each-finding-onto-the-stage-schema", Kind: "cast"},
		{Ref: "stage-each-finding", Kind: "plugin", Action: "db.stages.upsert"},
	}
	live := []SettingsNode{
		{Ref: "clone-the-repository", Kind: "plugin", Action: "github.repo.clone", SettingsID: "nset_gh"},
		{Ref: "list-the-recent-commits", Kind: "plugin", Action: "github.activity.list", SettingsID: "nset_gh"},
		{Ref: "map-each-finding-onto-the-stage", Kind: "cast", SettingsID: "nset_llm"},
		{Ref: "stage-each-finding", Kind: "plugin", Action: "db.stages.upsert"},
	}
	got := MatchSettingsNodes(pkg, live)
	if got["map-each-finding-onto-the-stage-schema"].SettingsID != "nset_llm" {
		t.Fatalf("the truncated ref was not matched: %+v", got["map-each-finding-onto-the-stage-schema"])
	}
	if got["clone-the-repository"].SettingsID != "nset_gh" || got["stage-each-finding"].SettingsID != "" {
		t.Errorf("exact refs = %+v", got)
	}
}

// A node the author renamed on the canvas keeps neither ref; the only thing
// left is its kind and action, and its position among the nodes that share
// them. Two nodes calling the same action must not both take the first match.
func TestMatchSettingsNodesByPosition(t *testing.T) {
	pkg := []SettingsNode{
		{Ref: "first-query", Kind: "plugin", Action: "osquery.query"},
		{Ref: "second-query", Kind: "plugin", Action: "osquery.query"},
	}
	live := []SettingsNode{
		{Ref: "renamed-a", Kind: "plugin", Action: "osquery.query", SettingsID: "nset_a"},
		{Ref: "renamed-b", Kind: "plugin", Action: "osquery.query", SettingsID: "nset_b"},
	}
	got := MatchSettingsNodes(pkg, live)
	if got["first-query"].SettingsID != "nset_a" || got["second-query"].SettingsID != "nset_b" {
		t.Fatalf("positional match = %+v", got)
	}
}

// A node that is simply not there any more matches nothing — it must not
// borrow another action's binding.
func TestMatchSettingsNodesLeavesGoneNodesUnmatched(t *testing.T) {
	pkg := []SettingsNode{{Ref: "review", Kind: "plugin", Action: "claude.agent.run"}}
	live := []SettingsNode{{Ref: "clone", Kind: "plugin", Action: "github.repo.clone", SettingsID: "nset_gh"}}
	if got := MatchSettingsNodes(pkg, live); len(got) != 0 {
		t.Fatalf("matched across actions: %+v", got)
	}
}
