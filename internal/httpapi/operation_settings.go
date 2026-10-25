package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/Venapce/venapce-api/internal/flomorphic"
	"github.com/Venapce/venapce-api/internal/operations"
)

// What an operation package cannot bring with it: the plugins its flows call,
// and the node settings profiles their nodes have to be bound to.
//
// A package travels as flow files. Those files name the plugin *action* each
// node calls (portable) but can carry no profile and no plugin id (both are
// addresses on one install), and installing a flow overwrites the whole
// document — so every install, including a re-install after an upgrade, leaves
// those nodes unbound on the canvas. Nothing in the package can fix that; the
// operator has to pick a profile per node in FloMorphic.
//
// So the import wizard says it up front, the way it already asks for the
// manifest's parameters: here is the plugin this package needs, here is
// whether this FloMorphic has it, here are the nodes whose profile you will
// have to pick, and here is the flow to pick them in. The same report is
// served for an installed operation, where it also knows which nodes *are*
// bound — the list to work through after an install or a re-install.

// settingsStatus values, worst first.
const (
	settingsUnknown      = "unknown"      // FloMorphic did not answer; nothing could be checked
	settingsNotInstalled = "notInstalled" // no plugin here provides the node's action
	settingsNoProfile    = "noProfile"    // nothing to bind: the bucket holds no profile
	settingsUnbound      = "unbound"      // profiles exist; this node is not bound to one
	settingsOK           = "ok"           // bound, or nothing to configure
)

// settingsNeed is one node whose settings profile the operator has to pick.
type settingsNeed struct {
	FlowKey   string `json:"flowKey,omitempty"`
	FlowTitle string `json:"flowTitle,omitempty"`
	// FlowID is set once the flow is installed — the flow to open in FloMorphic
	// to bind this node.
	FlowID string `json:"flowId,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// Node is what the node is called on the canvas (its title, else its ref).
	Node   string `json:"node"`
	Kind   string `json:"kind,omitempty"`
	Action string `json:"action,omitempty"`
	// Plugin is the name of what provides the node: the plugin as this install
	// registered it, the plugin the manifest declared, or the builtin's label.
	Plugin string `json:"plugin,omitempty"`
	// NodeUniqID is the profile bucket on the install (ext:<pluginId>, or the
	// node kind for a builtin). Empty when the plugin is not installed, since
	// the bucket is its per-install id.
	NodeUniqID string `json:"nodeUniqId,omitempty"`
	// Profiles are the profiles already in that bucket, by title — what the
	// node's picker will offer.
	Profiles []string `json:"profiles,omitempty"`
	// Required is false for a node that runs unbound (an http node's request is
	// configured on the node itself); those are listed but never counted as
	// pending.
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	BoundTo  string `json:"boundTo,omitempty"`
	// Note is what the manifest's requires.settings says this profile must be.
	Note string `json:"note,omitempty"`
	// Href opens the flow on the FloMorphic canvas (the node drawer is where a
	// profile is picked); SettingsHref is FloMorphic's Node Settings page,
	// where a profile is created.
	Href         string `json:"href,omitempty"`
	SettingsHref string `json:"settingsHref,omitempty"`
}

// pluginNeed is one plugin the package's flows call, as this install has it.
type pluginNeed struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
	// Missing are the actions no plugin here provides.
	Missing []string `json:"missing,omitempty"`
	// Nodes is how many nodes of the package call it.
	Nodes int `json:"nodes"`
	// Venapce marks this panel's own plugin, which takes no profile: it reads
	// its configuration from here.
	Venapce bool `json:"venapce,omitempty"`
	// Builtin marks a FloMorphic node kind rather than an imported plugin.
	Builtin  bool   `json:"builtin,omitempty"`
	Profiles int    `json:"profiles"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Note     string `json:"note,omitempty"`
	Repo     string `json:"repo,omitempty"`
	// Href is where to get it (the plugin's repository), SettingsHref where its
	// profiles are kept on this FloMorphic.
	Href         string `json:"href,omitempty"`
	SettingsHref string `json:"settingsHref,omitempty"`
}

// settingsPlan is the whole report for one package.
type settingsPlan struct {
	Plugins []pluginNeed   `json:"plugins"`
	Needs   []settingsNeed `json:"needs"`
	// Pending is how many required needs are not satisfied.
	Pending int `json:"pending"`
	// Missing is how many plugins this FloMorphic does not have.
	Missing int `json:"missing"`
	// Known is false when FloMorphic could not be asked — every verdict is then
	// `unknown` and the panel must not claim anything is wrong.
	Known bool `json:"known"`
	// Live is true when the bound flows were read back, so `ok` means bound on
	// the canvas; false in the import preview, where nothing is installed yet.
	Live     bool   `json:"live"`
	PanelURL string `json:"panelUrl,omitempty"`
}

// builtinLabels name the profile-bearing node kinds the way FloMorphic's
// palette does, so a need reads as the operator's canvas does.
var builtinLabels = map[string]string{
	"llm":         "LLM",
	"ai-decision": "AI Decision",
	"cast":        "Cast / Mapping",
	"mcp":         "MCP (LLM mode)",
	"http":        "HTTP request",
	"hitl":        "Human task (chat provider)",
}

// settingsFacts is everything the plan is computed against, gathered once per
// request. Kept separate from the building so the rules below are testable
// without a FloMorphic.
type settingsFacts struct {
	// actions is the install's plugin action table; known says it answered.
	actions map[string]flomorphic.PluginAction
	known   bool
	// profiles is bucket → the profiles in it, for the buckets the nodes
	// resolved to.
	profiles map[string][]flomorphic.SettingsProfile
	// live is flowKey → node ref → the settingsId the bound flow carries now.
	// Nil for a preview, where no flow exists yet.
	live     map[string]map[string]string
	binds    map[string]binding
	panelURL string
}

// panel is the panel address without its trailing slash — an operator typing
// it into Settings is as likely to end it with one as not.
func (f settingsFacts) panel() string {
	return strings.TrimRight(strings.TrimSpace(f.panelURL), "/")
}

func (f settingsFacts) flowURL(flowID string) string {
	if f.panel() == "" || flowID == "" {
		return ""
	}
	return f.panel() + "/#/workflows/" + flowID
}

func (f settingsFacts) settingsURL() string {
	if f.panel() == "" {
		return ""
	}
	return f.panel() + "/#/node-settings"
}

func profileTitles(list []flomorphic.SettingsProfile) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		t := strings.TrimSpace(p.Title)
		if t == "" {
			t = p.ID
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// buildSettingsPlan turns a package plus what is known about the install into
// the report. It decides nothing about presentation beyond the wording of each
// verdict, which is the same whether it is read in the import wizard or on the
// operation page.
func buildSettingsPlan(b *operations.Bundle, f settingsFacts) settingsPlan {
	m := b.Manifest
	nodes := b.SettingsNodes()
	plan := settingsPlan{Known: f.known, Live: f.live != nil, PanelURL: f.panel()}

	// Plugin names the manifest declared, by action — a plugin this install
	// does not have has no name of its own here, and "the github plugin" beats
	// "whatever provides github.repo.clone".
	declaredBy := map[string]operations.PluginReq{}
	for _, p := range manifestPlugins(m) {
		for _, a := range p.Actions {
			declaredBy[a] = p
		}
	}

	type group struct {
		need  pluginNeed
		seen  map[string]bool
		order []string
	}
	groups := map[string]*group{}
	var groupOrder []string
	addAction := func(name string, mk func(*pluginNeed), action string, missing bool) *group {
		g := groups[name]
		if g == nil {
			g = &group{need: pluginNeed{Name: name}, seen: map[string]bool{}}
			mk(&g.need)
			groups[name] = g
			groupOrder = append(groupOrder, name)
		}
		if action != "" && !g.seen[action] {
			g.seen[action] = true
			g.order = append(g.order, action)
			g.need.Actions = append(g.need.Actions, action)
			if missing {
				g.need.Missing = append(g.need.Missing, action)
			}
		}
		g.need.Nodes++
		return g
	}

	for _, n := range nodes {
		need := settingsNeed{
			FlowKey:  n.FlowKey,
			Ref:      n.Ref,
			Node:     orDefault(n.Title, n.Ref),
			Kind:     n.Kind,
			Action:   n.Action,
			Required: !operations.OptionalSettingsProfile(n.Kind),
		}
		if fs := m.Flow(n.FlowKey); fs != nil {
			need.FlowTitle = orDefault(fs.Title, fs.Key)
		}
		if bnd, ok := f.binds[n.FlowKey]; ok {
			need.FlowID = bnd.FlowID
		}
		for _, r := range m.SettingsNotesFor(n) {
			need.Note = joinNote(need.Note, r.Note)
		}

		venapce := n.Kind == "plugin" && operations.IsVenapceAction(n.Action)
		pluginID, pluginName := "", ""
		installed := true
		switch {
		case n.Kind != "plugin":
			// A builtin: its bucket is the kind itself, always present.
			pluginName = orDefault(builtinLabels[n.Kind], n.Kind)
			need.Plugin = pluginName
		default:
			if a, ok := f.actions[n.Action]; ok {
				pluginID, pluginName = a.PluginID, a.PluginName
			} else {
				installed = false
				pluginName = declaredBy[n.Action].Name
				if pluginName == "" {
					pluginName = strings.SplitN(n.Action, ".", 2)[0]
				}
			}
			if venapce {
				pluginName = "venapce"
			}
			need.Plugin = pluginName
		}

		switch {
		case n.Kind == "plugin" && !f.known:
			need.Status = settingsUnknown
			need.Detail = "FloMorphic could not be asked which plugins it has — check Settings → FloMorphic."
		case venapce:
			// venapce's own actions take their configuration from this panel
			// (osctrl access, the database), never from a profile on the node.
			need.Required = false
			need.Status = settingsOK
			need.Detail = "Provided by the venapce plugin — it reads its configuration from this panel, so there is nothing to bind."
		case !installed:
			need.Status = settingsNotInstalled
			need.Detail = fmt.Sprintf("No plugin on this FloMorphic provides %s. Install the %s plugin, sync its actions, then install this flow and bind the node.", n.Action, pluginName)
		default:
			bucket := flomorphic.SettingsBucket(n.Kind, pluginID)
			need.NodeUniqID = bucket
			profiles := f.profiles[bucket]
			need.Profiles = profileTitles(profiles)
			bound := ""
			if f.live != nil {
				bound = f.live[n.FlowKey][n.Ref]
			}
			switch {
			case bound != "":
				need.Status = settingsOK
				need.BoundTo = bound
				for _, p := range profiles {
					if p.ID == bound {
						need.BoundTo = orDefault(strings.TrimSpace(p.Title), p.ID)
					}
				}
				need.Detail = fmt.Sprintf("Bound to the %q profile.", need.BoundTo)
				if !profileHasID(profiles, bound) {
					// A profile that was deleted, or an id that rode in from the
					// install the package was exported on.
					need.Status = settingsUnbound
					need.BoundTo = ""
					need.Detail = fmt.Sprintf("Bound to a profile this FloMorphic does not have (%s) — open the flow and pick one of its %s profiles.", bound, pluginName)
				}
			case len(profiles) == 0:
				need.Status = settingsNoProfile
				need.Detail = fmt.Sprintf("This FloMorphic has no %s profile yet — create one in FloMorphic → Node Settings, then bind it on the node.", pluginName)
			case f.live != nil && need.FlowID != "":
				need.Status = settingsUnbound
				need.Detail = fmt.Sprintf("Not bound. Open the flow and pick one of the %d %s profile(s) on this node.", len(profiles), pluginName)
			case f.live == nil && need.FlowID != "":
				// The upgrade path: the flow is bound already, and installing it
				// again writes the whole document — whatever was bound here is
				// gone, which is the one thing the operator has to be told
				// before they press the button.
				need.Status = settingsUnbound
				need.Detail = fmt.Sprintf("This flow is already installed. Re-installing writes the file again and clears this node's profile — open the flow afterwards and bind it to one of the %d %s profile(s).", len(profiles), pluginName)
			default:
				need.Status = settingsUnbound
				need.Detail = fmt.Sprintf("After installing the flow, open it in FloMorphic and bind this node to one of the %d %s profile(s).", len(profiles), pluginName)
			}
		}

		if n.SettingsID != "" && f.live == nil {
			need.Detail += " The package file carries the profile the author had bound; that id belongs to their install and resolves to nothing here."
		}
		need.Href = f.flowURL(need.FlowID)
		need.SettingsHref = f.settingsURL()
		if need.Required && need.Status != settingsOK {
			plan.Pending++
		}
		plan.Needs = append(plan.Needs, need)

		// The plugin row this node belongs to.
		if n.Kind == "plugin" {
			decl := declaredBy[n.Action]
			g := addAction(pluginName, func(p *pluginNeed) {
				p.Venapce = venapce
				p.Repo, p.Note = decl.Repo, decl.Note
			}, n.Action, !installed)
			if g.need.Note == "" {
				g.need.Note = decl.Note
			}
			if g.need.Repo == "" {
				g.need.Repo = decl.Repo
			}
			g.need.SettingsHref = f.settingsURL()
			if pluginID != "" {
				g.need.Profiles = len(f.profiles[flomorphic.SettingsBucket(n.Kind, pluginID)])
			}
		} else {
			g := addAction(pluginName, func(p *pluginNeed) { p.Builtin = true }, "", false)
			g.need.Profiles = len(f.profiles[flomorphic.SettingsBucket(n.Kind, "")])
			g.need.SettingsHref = f.settingsURL()
		}
	}

	// Declared plugins nothing in the flows calls: the author meant them, so
	// they are checked too rather than passed over.
	for _, p := range manifestPlugins(m) {
		name := p.Name
		if len(p.Actions) > 0 && operations.IsVenapceAction(p.Actions[0]) {
			name = "venapce"
		}
		if _, ok := groups[name]; ok {
			continue
		}
		var missing []string
		for _, a := range p.Actions {
			if _, ok := f.actions[a]; !ok {
				missing = append(missing, a)
			}
		}
		g := &group{need: pluginNeed{Name: name, Actions: p.Actions, Missing: missing, Repo: p.Repo, Note: p.Note, SettingsHref: f.settingsURL()}, seen: map[string]bool{}}
		groups[name] = g
		groupOrder = append(groupOrder, name)
	}

	for _, name := range groupOrder {
		g := groups[name]
		p := g.need
		// A builtin kind is grouped with no action name at all; the field is
		// listed, so it travels as [] rather than null.
		if p.Actions == nil {
			p.Actions = []string{}
		}
		switch {
		case !f.known:
			p.Status = settingsUnknown
			p.Detail = "FloMorphic did not answer — its plugin list could not be read."
		case p.Builtin:
			p.Status = settingsOK
			p.Detail = fmt.Sprintf("A FloMorphic builtin — %d profile(s) here.", p.Profiles)
		case len(p.Missing) == 0:
			p.Status = settingsOK
			p.Detail = fmt.Sprintf("Installed, provides %s.", strings.Join(p.Actions, ", "))
			if p.Venapce {
				p.Detail = "This panel's own plugin — " + p.Detail
			}
		case len(p.Missing) == len(p.Actions):
			p.Status = settingsNotInstalled
			p.Detail = "Not installed on this FloMorphic: nothing provides " + strings.Join(p.Missing, ", ") + "."
		default:
			p.Status = settingsNotInstalled
			p.Detail = "Installed, but it does not provide " + strings.Join(p.Missing, ", ") + " — sync its actions, or install a newer version."
		}
		if p.Status == settingsNotInstalled {
			switch {
			case p.Venapce:
				p.Detail += " Register venapce as a FloMorphic plugin and refresh it (Settings → FloMorphic)."
			case p.Repo != "":
				p.Detail += " Install it from " + p.Repo + " and sync it in FloMorphic → Extensions."
				p.Href = p.Repo
			default:
				p.Detail += " Install the plugin in FloMorphic → Extensions and sync its actions."
			}
			plan.Missing++
		}
		if p.Note != "" && p.Status != settingsNotInstalled {
			p.Detail += " " + p.Note
		}
		plan.Plugins = append(plan.Plugins, p)
	}

	// A requires.settings entry no node could be matched to is still the
	// operator's to meet; it is reported as its own need.
	for _, r := range m.UnmatchedSettings(nodes) {
		label := strings.TrimSpace(r.For)
		if label == "" {
			label = orDefault(r.Kind, "a node") + " settings profile"
		}
		plan.Needs = append(plan.Needs, settingsNeed{
			Node:         label,
			Kind:         r.Kind,
			Required:     true,
			Status:       settingsUnbound,
			Detail:       "The manifest asks for this profile but names no node of the package it belongs to — bind it on the node the README points at.",
			Note:         r.Note,
			SettingsHref: f.settingsURL(),
		})
		plan.Pending++
	}

	if plan.Plugins == nil {
		plan.Plugins = []pluginNeed{}
	}
	if plan.Needs == nil {
		plan.Needs = []settingsNeed{}
	}
	return plan
}

func manifestPlugins(m *operations.Manifest) []operations.PluginReq {
	if m.Requires == nil {
		return nil
	}
	return m.Requires.Plugins
}

func profileHasID(list []flomorphic.SettingsProfile, id string) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

func joinNote(have, add string) string {
	add = strings.TrimSpace(add)
	switch {
	case add == "":
		return have
	case have == "":
		return add
	default:
		return have + " " + add
	}
}

// settingsPlanFor gathers the facts and builds the plan. `live` reads each
// bound flow back from FloMorphic, so the report can say which nodes are
// already bound — the operation page wants that, the import preview has
// nothing installed to read.
func (s *Server) settingsPlanFor(ctx context.Context, b *operations.Bundle, binds map[string]binding, e estate, live bool) settingsPlan {
	f := settingsFacts{
		actions:  e.actions,
		known:    e.actionsKnown,
		profiles: map[string][]flomorphic.SettingsProfile{},
		binds:    binds,
		panelURL: strings.TrimRight(s.flo.Access().PanelURL, "/"),
	}
	client := s.flo.Get()
	if client == nil {
		f.known = false
		return buildSettingsPlan(b, f)
	}

	nodes := b.SettingsNodes()
	// One listing per bucket the nodes resolve to, not per node.
	for _, n := range nodes {
		pluginID := ""
		if n.Kind == "plugin" {
			a, ok := e.actions[n.Action]
			if !ok || operations.IsVenapceAction(n.Action) {
				continue
			}
			pluginID = a.PluginID
		}
		bucket := flomorphic.SettingsBucket(n.Kind, pluginID)
		if bucket == "" {
			continue
		}
		if _, done := f.profiles[bucket]; done {
			continue
		}
		list, err := client.ListSettingsProfiles(ctx, bucket)
		if err != nil {
			// The bucket is unreadable, not empty: an empty list would be read as
			// "create a profile". Leave it out and let the node report `unbound`.
			continue
		}
		f.profiles[bucket] = list
	}

	if live {
		f.live = map[string]map[string]string{}
		for key, bnd := range binds {
			if bnd.FlowID == "" {
				continue
			}
			doc, err := client.ExportFlow(ctx, bnd.FlowID)
			if err != nil {
				continue
			}
			// The flow as the install holds it: its node refs are re-derived on
			// import (and capped), so the package's nodes are matched to it
			// rather than looked up by ref.
			var pkg []operations.SettingsNode
			for _, n := range nodes {
				if n.FlowKey == key {
					pkg = append(pkg, n)
				}
			}
			bound := map[string]string{}
			for ref, live := range operations.MatchSettingsNodes(pkg, operations.SettingsNodesIn(key, doc)) {
				if live.SettingsID != "" {
					bound[ref] = live.SettingsID
				}
			}
			f.live[key] = bound
		}
	}
	return buildSettingsPlan(b, f)
}

// GET /api/operations/:id/settings — the plugin and node-settings report for
// an installed operation, with the bound flows read back so it says which
// nodes still have no profile. Separate from the operation detail because it
// is several FloMorphic round trips, and the page can fetch it alongside.
func (s *Server) getOperationSettings(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	b, err := s.loadBundle(op)
	if err != nil {
		return err
	}
	e := s.gatherEstate(c.Context())
	return c.JSON(s.settingsPlanFor(c.Context(), b, bindingsOf(op), e, true))
}
