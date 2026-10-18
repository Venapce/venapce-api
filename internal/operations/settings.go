package operations

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Node settings profiles — the half of an operation's configuration that no
// package can carry.
//
// A FloMorphic node that reaches a provider takes its credentials and its
// endpoint from a *node settings profile*: a named key/value bundle stored on
// the install and bound to the node by id (`data.settingsId`). The profile id
// is an address on one install, so a package cannot ship it — and the file a
// package ships is overwritten into the flow on every install, which is why a
// re-install leaves those nodes unbound again.
//
// What a package *can* do is say which nodes need one. That is what this file
// reads out of the flow files, and what the import wizard shows the operator
// before they install: the plugins the flows call (and whether this FloMorphic
// has them), and the nodes whose profile they will have to pick on the canvas.
//
// The kinds below mirror FloMorphic's node catalog (the entries flagged
// `plugin: true`, which is what makes its drawer show a profile picker) plus
// `hitl`, whose chat provider is bound the same way by reference.

// SettingsNode is one node of a workflow document that binds a settings
// profile.
type SettingsNode struct {
	FlowKey string `json:"flowKey"`
	// Ref is the node's name in the portable document. The same node keeps it
	// when the document is imported and exported again, so it is how a flow
	// read back from the install is matched to the package's node.
	Ref   string `json:"ref"`
	Title string `json:"title,omitempty"`
	Kind  string `json:"kind"`
	// Action is the plugin method a `plugin` node calls — the portable key that
	// says which plugin the profile has to belong to.
	Action string `json:"action,omitempty"`
	// SettingsID is the profile the document carries. In a package file it is a
	// leftover from the install that exported it and resolves to nothing here;
	// in a flow read back from this install it is the live binding.
	SettingsID string `json:"settingsId,omitempty"`
}

// NeedsSettingsProfile reports whether a node of this kind binds a settings
// profile. `data` is only consulted for the kinds whose mode decides it.
func NeedsSettingsProfile(kind string, data map[string]any) bool {
	switch kind {
	case "plugin", "llm", "ai-decision", "cast", "http", "hitl":
		return true
	case "mcp":
		// Only the LLM-backed mode takes a provider profile; a plain MCP client
		// node is configured by its own fields.
		mode, _ := data["mcpMode"].(string)
		return mode == "llm"
	}
	return false
}

// OptionalSettingsProfile reports whether a node of this kind runs without a
// profile. An `http` node's request is configured on the node itself and a
// profile only factors out a shared base URL / auth; every other
// profile-bearing kind reaches a provider it cannot reach unbound.
func OptionalSettingsProfile(kind string) bool { return kind == "http" }

// SettingsNodesIn reads a portable workflow document (the shape a package
// ships and `GET /flow/id/:id/export` answers) and returns the nodes that bind
// a settings profile, in document order. An unreadable document yields none —
// Bundle.Check is what reports a broken file.
func SettingsNodesIn(flowKey string, doc []byte) []SettingsNode {
	var d struct {
		Nodes []struct {
			Ref   string         `json:"ref"`
			Kind  string         `json:"kind"`
			Title string         `json:"title"`
			Data  map[string]any `json:"data"`
		} `json:"nodes"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return nil
	}
	var out []SettingsNode
	for _, n := range d.Nodes {
		if !NeedsSettingsProfile(n.Kind, n.Data) {
			continue
		}
		action, _ := n.Data["action"].(string)
		id, _ := n.Data["settingsId"].(string)
		out = append(out, SettingsNode{
			FlowKey:    flowKey,
			Ref:        strings.TrimSpace(n.Ref),
			Title:      strings.TrimSpace(n.Title),
			Kind:       n.Kind,
			Action:     strings.TrimSpace(action),
			SettingsID: strings.TrimSpace(id),
		})
	}
	return out
}

// SettingsNodes lists every profile-bearing node of the package, flow by flow
// in manifest order.
func (b *Bundle) SettingsNodes() []SettingsNode {
	var out []SettingsNode
	for _, f := range b.Manifest.Flows {
		text, ok := b.Files[f.File]
		if !ok {
			continue
		}
		out = append(out, SettingsNodesIn(f.Key, []byte(text))...)
	}
	return out
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeTerm(s string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
}

// Names reports whether a requires.settings entry's `for` names this node. The
// field is prose — a comma or slash separated list of node titles, as an author
// writes it — so each term is matched loosely against the node's title, its
// ref, its action and its kind.
func (r SettingsReq) Names(n SettingsNode) bool {
	if strings.TrimSpace(r.For) == "" {
		return false
	}
	want := map[string]bool{}
	for _, part := range strings.FieldsFunc(r.For, func(c rune) bool { return c == ',' || c == ';' || c == '/' || c == '\n' }) {
		if t := normalizeTerm(part); t != "" {
			want[t] = true
		}
	}
	for _, have := range []string{n.Title, n.Ref, n.Action, n.Kind} {
		if t := normalizeTerm(have); t != "" && want[t] {
			return true
		}
	}
	// A ref is derived from the title and truncated, so an author's full title
	// may be longer than the ref it became ("Per-node nginx config files
	// (docker)" → "per-node-nginx-config-files-dock"). Accept a prefix match
	// against the ref, which is the only lossy side.
	ref := normalizeTerm(n.Ref)
	for t := range want {
		if ref != "" && len(t) > len(ref) && strings.HasPrefix(t, ref) {
			return true
		}
	}
	return false
}

// SettingsNotesFor returns the manifest's requires.settings entries that name
// this node — the author's word on what its profile has to be.
func (m *Manifest) SettingsNotesFor(n SettingsNode) []SettingsReq {
	var out []SettingsReq
	if m.Requires == nil {
		return nil
	}
	for _, r := range m.Requires.Settings {
		if r.Names(n) {
			out = append(out, r)
		}
	}
	return out
}

// UnmatchedSettings returns the requires.settings entries that name no node of
// the package — a requirement the author wrote down that nothing in the flows
// could be matched to. They are still the operator's to meet, so they are
// reported rather than dropped.
func (m *Manifest) UnmatchedSettings(nodes []SettingsNode) []SettingsReq {
	if m.Requires == nil {
		return nil
	}
	var out []SettingsReq
	for _, r := range m.Requires.Settings {
		named := false
		for _, n := range nodes {
			if r.Names(n) {
				named = true
				break
			}
		}
		if !named {
			out = append(out, r)
		}
	}
	return out
}

// MatchSettingsNodes pairs the package's profile-bearing nodes with the same
// nodes of the flow as an install holds it, returning the live node for each
// package node's ref.
//
// A ref is not a stable key across an install: FloMorphic re-derives it from
// the node's title and caps its length, so "map-each-finding-onto-the-stage-schema"
// comes back as "map-each-finding-onto-the-stage". So the match is tried in
// order of how much it proves: the same ref, then a ref that is a truncation of
// the other with the same kind and action, then position within the nodes of
// that kind and action. Each live node is claimed once.
func MatchSettingsNodes(pkg, live []SettingsNode) map[string]SettingsNode {
	out := map[string]SettingsNode{}
	taken := make([]bool, len(live))
	claim := func(ref string, i int) {
		out[ref] = live[i]
		taken[i] = true
	}
	same := func(a, b SettingsNode) bool { return a.Kind == b.Kind && a.Action == b.Action }

	for _, p := range pkg {
		for i, l := range live {
			if !taken[i] && p.Ref != "" && l.Ref == p.Ref {
				claim(p.Ref, i)
				break
			}
		}
	}
	// A truncated ref: one side is a prefix of the other. Short refs are left to
	// the positional pass, where a wrong guess costs nothing but a row that
	// reads "not bound".
	const minPrefix = 8
	for _, p := range pkg {
		if _, done := out[p.Ref]; done {
			continue
		}
		for i, l := range live {
			if taken[i] || !same(p, l) || len(p.Ref) < minPrefix || len(l.Ref) < minPrefix {
				continue
			}
			if strings.HasPrefix(p.Ref, l.Ref) || strings.HasPrefix(l.Ref, p.Ref) {
				claim(p.Ref, i)
				break
			}
		}
	}
	for _, p := range pkg {
		if _, done := out[p.Ref]; done {
			continue
		}
		for i, l := range live {
			if !taken[i] && same(p, l) {
				claim(p.Ref, i)
				break
			}
		}
	}
	return out
}
