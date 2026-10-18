package httpapi

import (
	"strings"
	"testing"

	"github.com/Venapce/venapce-api/internal/flomorphic"
	"github.com/Venapce/venapce-api/internal/operations"
)

const planFlow = `{
  "flomorphic": {"kind": "workflow"},
  "nodes": [
    {"ref": "start", "kind": "startNode", "title": "Start"},
    {"ref": "clone", "kind": "plugin", "title": "Clone the repository", "data": {"action": "github.repo.clone"}},
    {"ref": "review", "kind": "plugin", "title": "Review the commits with Claude", "data": {"action": "claude.agent.run", "settingsId": "nset_authors"}},
    {"ref": "map", "kind": "cast", "title": "Map each finding", "data": {"mappings": []}},
    {"ref": "stage", "kind": "plugin", "title": "Stage each finding", "data": {"action": "db.stages.upsert"}}
  ]
}`

func planBundle(t *testing.T) *operations.Bundle {
	t.Helper()
	m, errs := operations.ParseManifest([]byte(`{
      "schema": 1, "id": "commit-review", "name": "Commit review", "version": "1.0.0",
      "flows": [{"key": "review", "file": "flows/review.flow.json", "title": "Commit review", "role": "entry"}],
      "requires": {"plugins": [
        {"name": "Github-Dev", "actions": ["github.repo.clone"], "repo": "https://github.com/x/github-plugin"},
        {"name": "Claude", "actions": ["claude.agent.run"]},
        {"name": "venapce", "actions": ["db.stages.upsert"]}
      ], "settings": [
        {"kind": "other", "for": "Review the commits with Claude", "note": "A Claude profile whose workspace holds the clone root."}
      ]}
    }`))
	if m == nil || len(errs) > 0 {
		t.Fatalf("manifest: %v", errs)
	}
	return &operations.Bundle{Manifest: m, Files: map[string]string{"flows/review.flow.json": planFlow}}
}

func needOf(t *testing.T, p settingsPlan, ref string) settingsNeed {
	t.Helper()
	for _, n := range p.Needs {
		if n.Ref == ref {
			return n
		}
	}
	t.Fatalf("no need for node %q in %+v", ref, p.Needs)
	return settingsNeed{}
}

func pluginOf(t *testing.T, p settingsPlan, name string) pluginNeed {
	t.Helper()
	for _, pl := range p.Plugins {
		if pl.Name == name {
			return pl
		}
	}
	t.Fatalf("no plugin row %q in %+v", name, p.Plugins)
	return pluginNeed{}
}

func actions(pairs ...[2]string) map[string]flomorphic.PluginAction {
	out := map[string]flomorphic.PluginAction{}
	for _, p := range pairs {
		out[p[0]] = flomorphic.PluginAction{Action: p[0], PluginID: p[1], PluginName: p[1]}
	}
	return out
}

// The import preview, on an install that has both plugins and a profile for
// each: nothing is bound yet (no flow exists), so every node that reaches a
// provider is pending and says where it will be bound.
func TestPlanPreviewNamesEveryNodeToBind(t *testing.T) {
	b := planBundle(t)
	p := buildSettingsPlan(b, settingsFacts{
		known:   true,
		actions: actions([2]string{"github.repo.clone", "github"}, [2]string{"claude.agent.run", "claude"}, [2]string{"db.stages.upsert", "venapce"}),
		profiles: map[string][]flomorphic.SettingsProfile{
			"ext:github": {{ID: "nset_gh", Title: "acme GitHub"}},
			"ext:claude": {{ID: "nset_cl", Title: "Claude CLI"}},
			"cast":       {{ID: "nset_llm", Title: "gemini"}},
		},
		panelURL: "http://localhost:8027",
	})

	if p.Live {
		t.Error("a preview is not live")
	}
	// The three provider nodes: the two plugin ones and the cast builtin. The
	// venapce node is not one — it takes no profile.
	if p.Pending != 3 {
		t.Fatalf("pending = %d, want 3: %+v", p.Pending, p.Needs)
	}
	gh := needOf(t, p, "clone")
	if gh.Status != settingsUnbound || gh.NodeUniqID != "ext:github" || gh.Plugin != "github" {
		t.Errorf("clone node = %+v", gh)
	}
	if len(gh.Profiles) != 1 || gh.Profiles[0] != "acme GitHub" {
		t.Errorf("clone node profiles = %v", gh.Profiles)
	}
	if gh.SettingsHref != "http://localhost:8027/#/node-settings" {
		t.Errorf("settings href = %q", gh.SettingsHref)
	}
	if gh.FlowID != "" || gh.Href != "" {
		t.Errorf("nothing is installed yet, so there is no flow to link: %+v", gh)
	}

	// The node the author had bound: the preview must say the id in the file is
	// theirs, not a binding that carries over.
	claude := needOf(t, p, "review")
	if claude.Status != settingsUnbound {
		t.Errorf("claude node status = %q", claude.Status)
	}
	if claude.Note == "" {
		t.Error("the manifest's requires.settings note should reach the node it names")
	}
	if !strings.Contains(claude.Detail, "belongs to their install") {
		t.Errorf("the author's stale settingsId is not explained: %q", claude.Detail)
	}

	// venapce's own actions configure themselves from this panel.
	stage := needOf(t, p, "stage")
	if stage.Status != settingsOK || stage.Required {
		t.Errorf("venapce node = %+v", stage)
	}
	if v := pluginOf(t, p, "venapce"); v.Status != settingsOK || !v.Venapce {
		t.Errorf("venapce plugin row = %+v", v)
	}
}

// The case the operator hits most: the package calls a plugin this FloMorphic
// does not have. That has to be a warning before the install, not a run-time
// surprise — and it must name the plugin and where to get it.
func TestPlanWarnsAboutPluginsNotInstalled(t *testing.T) {
	b := planBundle(t)
	p := buildSettingsPlan(b, settingsFacts{
		known:    true,
		actions:  actions([2]string{"db.stages.upsert", "venapce"}),
		profiles: map[string][]flomorphic.SettingsProfile{"cast": {{ID: "nset_llm", Title: "gemini"}}},
	})

	if p.Missing != 2 {
		t.Fatalf("missing plugins = %d, want 2 (github + claude): %+v", p.Missing, p.Plugins)
	}
	gh := needOf(t, p, "clone")
	if gh.Status != settingsNotInstalled {
		t.Fatalf("clone node status = %q, want %q", gh.Status, settingsNotInstalled)
	}
	if gh.Plugin != "Github-Dev" {
		t.Errorf("an uninstalled plugin is still named by the manifest: %q", gh.Plugin)
	}
	if gh.NodeUniqID != "" {
		t.Errorf("there is no profile bucket for a plugin that is not here: %q", gh.NodeUniqID)
	}
	row := pluginOf(t, p, "Github-Dev")
	if row.Status != settingsNotInstalled || row.Href != "https://github.com/x/github-plugin" {
		t.Errorf("github plugin row = %+v", row)
	}
	if !strings.Contains(row.Detail, "Not installed") {
		t.Errorf("plugin row detail = %q", row.Detail)
	}
}

// Nothing to bind: the install has the plugin but no profile in its bucket, so
// the operator has to create one first.
func TestPlanSaysWhenNoProfileExists(t *testing.T) {
	b := planBundle(t)
	p := buildSettingsPlan(b, settingsFacts{
		known:    true,
		actions:  actions([2]string{"github.repo.clone", "github"}, [2]string{"claude.agent.run", "claude"}, [2]string{"db.stages.upsert", "venapce"}),
		profiles: map[string][]flomorphic.SettingsProfile{},
	})
	gh := needOf(t, p, "clone")
	if gh.Status != settingsNoProfile || !strings.Contains(gh.Detail, "Node Settings") {
		t.Fatalf("clone node = %+v", gh)
	}
}

// After an install: the flows are read back, so a bound node reads as done and
// the rest are the list to work through. A re-install lands here with every
// node unbound again — the whole point of the report.
func TestPlanLiveSeparatesBoundFromUnbound(t *testing.T) {
	b := planBundle(t)
	facts := settingsFacts{
		known:   true,
		actions: actions([2]string{"github.repo.clone", "github"}, [2]string{"claude.agent.run", "claude"}, [2]string{"db.stages.upsert", "venapce"}),
		profiles: map[string][]flomorphic.SettingsProfile{
			"ext:github": {{ID: "nset_gh", Title: "acme GitHub"}},
			"ext:claude": {{ID: "nset_cl", Title: "Claude CLI"}},
			"cast":       {{ID: "nset_llm", Title: "gemini"}},
		},
		binds:    map[string]binding{"review": {FlowID: "flow-1", FlowTitle: "Commit review"}},
		live:     map[string]map[string]string{"review": {"clone": "nset_gh", "review": "nset_gone"}},
		panelURL: "http://localhost:8027/",
	}
	p := buildSettingsPlan(b, facts)

	if !p.Live {
		t.Error("the plan read the flows back")
	}
	gh := needOf(t, p, "clone")
	if gh.Status != settingsOK || gh.BoundTo != "acme GitHub" {
		t.Fatalf("bound node = %+v", gh)
	}
	if gh.FlowID != "flow-1" || gh.Href != "http://localhost:8027/#/workflows/flow-1" {
		t.Errorf("a bound flow must link to its canvas: %+v", gh)
	}
	// A settingsId that resolves to nothing here is not a binding.
	claude := needOf(t, p, "review")
	if claude.Status != settingsUnbound || claude.BoundTo != "" {
		t.Fatalf("stale binding = %+v", claude)
	}
	if !strings.Contains(claude.Detail, "does not have") {
		t.Errorf("stale binding detail = %q", claude.Detail)
	}
	cast := needOf(t, p, "map")
	if cast.Status != settingsUnbound || !strings.Contains(cast.Detail, "Open the flow") {
		t.Fatalf("unbound builtin = %+v", cast)
	}
	if p.Pending != 2 {
		t.Fatalf("pending = %d, want 2 (claude + cast): %+v", p.Pending, p.Needs)
	}
}

// FloMorphic unreachable: the report says so instead of claiming the plugins
// are missing.
func TestPlanUnknownWhenFlomorphicSilent(t *testing.T) {
	b := planBundle(t)
	p := buildSettingsPlan(b, settingsFacts{known: false})
	if p.Known {
		t.Fatal("nothing was known")
	}
	if p.Missing != 0 {
		t.Errorf("missing = %d, want 0 — an unanswered FloMorphic is not a missing plugin", p.Missing)
	}
	for _, n := range p.Needs {
		if n.Kind == "plugin" && n.Status != settingsUnknown {
			t.Errorf("node %q status = %q, want %q", n.Ref, n.Status, settingsUnknown)
		}
	}
	for _, pl := range p.Plugins {
		if pl.Status != settingsUnknown {
			t.Errorf("plugin %q status = %q", pl.Name, pl.Status)
		}
	}
}

// A requires.settings entry that names no node is still reported: the author
// wrote it down, so it is the operator's to meet.
func TestPlanKeepsUnmatchedSettingsRequirements(t *testing.T) {
	m, errs := operations.ParseManifest([]byte(`{
      "schema": 1, "id": "p", "name": "P", "version": "1.0.0",
      "flows": [{"key": "f", "file": "f.json", "role": "entry"}],
      "requires": {"settings": [{"kind": "osctrl", "for": "A node nothing matches", "note": "Needs the fleet profile."}]}
    }`))
	if m == nil || len(errs) > 0 {
		t.Fatalf("manifest: %v", errs)
	}
	b := &operations.Bundle{Manifest: m, Files: map[string]string{"f.json": `{"nodes":[{"ref":"a","kind":"js","title":"A"}]}`}}
	p := buildSettingsPlan(b, settingsFacts{known: true})
	if len(p.Needs) != 1 || p.Needs[0].Node != "A node nothing matches" || p.Needs[0].Note != "Needs the fleet profile." {
		t.Fatalf("needs = %+v", p.Needs)
	}
	if p.Pending != 1 {
		t.Errorf("pending = %d", p.Pending)
	}
}

// Upgrading an operation whose flows are already bound: the preview has to say
// that installing again clears the bindings, since that is what it does and
// the operator is about to press the button.
func TestPlanPreviewWarnsReinstallClearsBindings(t *testing.T) {
	b := planBundle(t)
	p := buildSettingsPlan(b, settingsFacts{
		known:   true,
		actions: actions([2]string{"github.repo.clone", "github"}, [2]string{"claude.agent.run", "claude"}, [2]string{"db.stages.upsert", "venapce"}),
		profiles: map[string][]flomorphic.SettingsProfile{
			"ext:github": {{ID: "nset_gh", Title: "acme GitHub"}},
			"ext:claude": {{ID: "nset_cl", Title: "Claude CLI"}},
			"cast":       {{ID: "nset_llm", Title: "gemini"}},
		},
		// Installed and bound, but this is a preview: no flow was read back.
		binds:    map[string]binding{"review": {FlowID: "flow-1"}},
		panelURL: "http://localhost:8027",
	})
	gh := needOf(t, p, "clone")
	if gh.Status != settingsUnbound || !strings.Contains(gh.Detail, "clears this node's profile") {
		t.Fatalf("upgrade preview = %+v", gh)
	}
	if gh.Href != "http://localhost:8027/#/workflows/flow-1" {
		t.Errorf("the flow it will overwrite should be linked: %q", gh.Href)
	}
}
