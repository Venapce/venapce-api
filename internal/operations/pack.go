package operations

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Packing: the reverse of importing. Flows built on the FloMorphic canvas are
// exported (portable documents) and wrapped into a package — a manifest
// derived from what the exports declare (their plugin actions → `requires`),
// a README scaffold that names the mission and each flow, and the flow files
// under flows/<key>.flow.json. The result installs like any other package
// (already bound to the flows it came from) and zips for a catalog.

// PackFlow is one flow the author picked, with the manifest fields only they
// can supply (role, step, subject…). Title / description default to the
// export's own title.
type PackFlow struct {
	FlowID      string   `json:"flowId"`
	Key         string   `json:"key"`
	Role        string   `json:"role"`
	Step        int      `json:"step,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Subject     []string `json:"subject,omitempty"`
	Schedule    string   `json:"schedule,omitempty"`
	Writes      []string `json:"writes,omitempty"`
}

// PackInput is what the author fills in; everything else is derived.
type PackInput struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Tags        []string             `json:"tags,omitempty"`
	Scale       []string             `json:"scale,omitempty"`
	Authors     []string             `json:"authors,omitempty"`
	License     string               `json:"license,omitempty"`
	Homepage    string               `json:"homepage,omitempty"`
	Params      map[string]ParamSpec `json:"params,omitempty"`
	Flows       []PackFlow           `json:"flows"`
	// A README written by the author; a scaffold is generated when empty.
	Readme string `json:"readme,omitempty"`
	// Extra requirements the exports cannot reveal (settings profiles, other
	// operations, datasets, an osctrl note).
	Requires *Requires `json:"requires,omitempty"`
}

// exportHeader is the part of a portable workflow document packing reads.
type exportHeader struct {
	Title   string `json:"title"`
	Plugins []struct {
		Name    string   `json:"name"`
		Actions []string `json:"actions"`
		Repo    string   `json:"repo,omitempty"`
		Ref     string   `json:"ref,omitempty"`
		Subdir  string   `json:"subdir,omitempty"`
	} `json:"plugins"`
	Nodes []struct {
		Kind string `json:"kind"`
		Data struct {
			Action string `json:"action"`
		} `json:"data"`
	} `json:"nodes"`
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns free text into a manifest slug ("Linux fleet HTTP audit" →
// "linux-fleet-http-audit").
func Slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-")
}

// Pack builds the bundle from the author's input and the exported documents
// (keyed by flow id). It also reports the manifest key each flow id was packed
// under, so an install can bind them. Validation errors come back as a list,
// like ParseManifest.
func Pack(in PackInput, exports map[string]json.RawMessage) (*Bundle, map[string]string, []string) {
	var errs []string
	keyOf := map[string]string{}
	if in.ID == "" {
		in.ID = Slug(in.Name)
	}
	if in.Version == "" {
		in.Version = "1.0.0"
	}
	if len(in.Flows) == 0 {
		errs = append(errs, "pick at least one flow")
	}

	files := map[string]string{}
	flows := make([]FlowSpec, 0, len(in.Flows))
	type plug struct {
		name, repo, ref, subdir string
		actions                 map[string]bool
	}
	plugins := map[string]*plug{}
	var plugOrder []string
	usesOsquery := false
	keys := map[string]bool{}
	for i, pf := range in.Flows {
		doc, ok := exports[pf.FlowID]
		if !ok {
			errs = append(errs, fmt.Sprintf("flows[%d]: no export for flow %q", i, pf.FlowID))
			continue
		}
		var h exportHeader
		if err := json.Unmarshal(doc, &h); err != nil {
			errs = append(errs, fmt.Sprintf("flows[%d]: export is not a workflow document: %v", i, err))
			continue
		}
		key := pf.Key
		if key == "" {
			key = Slug(firstNonEmpty(pf.Title, h.Title))
		}
		if key == "" || keys[key] {
			key = fmt.Sprintf("%s-%d", firstNonEmpty(key, "flow"), i+1)
		}
		keys[key] = true
		keyOf[pf.FlowID] = key
		role := pf.Role
		if role == "" {
			role = "entry"
		}
		spec := FlowSpec{
			Key: key, File: "flows/" + key + ".flow.json", Title: firstNonEmpty(pf.Title, h.Title),
			Description: pf.Description, Role: role, Step: pf.Step, Subject: pf.Subject, Schedule: pf.Schedule, Writes: pf.Writes,
		}
		if spec.Step == 0 {
			spec.Step = i + 1
		}
		flows = append(flows, spec)
		// Pretty-print the export: the file is meant to be read and diffed.
		var pretty bytes.Buffer
		if json.Indent(&pretty, doc, "", "  ") == nil {
			files[spec.File] = pretty.String() + "\n"
		} else {
			files[spec.File] = string(doc)
		}

		// What the flow calls: the export's own manifest names the plugin and
		// repo; the nodes are the ground truth for the action list.
		byAction := map[string]int{}
		for pi, p := range h.Plugins {
			for _, a := range p.Actions {
				byAction[a] = pi
			}
		}
		for _, n := range h.Nodes {
			a := strings.TrimSpace(n.Data.Action)
			if n.Kind != "plugin" || a == "" {
				continue
			}
			if strings.HasPrefix(a, "osquery.") || strings.HasPrefix(a, "osctrl.") {
				usesOsquery = true
			}
			name, repo, ref, subdir := "", "", "", ""
			if pi, ok := byAction[a]; ok {
				p := h.Plugins[pi]
				name, repo, ref, subdir = p.Name, p.Repo, p.Ref, p.Subdir
			}
			if IsVenapceAction(a) {
				name = "venapce"
			}
			if name == "" {
				name = strings.SplitN(a, ".", 2)[0]
			}
			g := plugins[name]
			if g == nil {
				g = &plug{name: name, repo: repo, ref: ref, subdir: subdir, actions: map[string]bool{}}
				plugins[name] = g
				plugOrder = append(plugOrder, name)
			}
			g.actions[a] = true
		}
	}

	req := Requires{}
	if in.Requires != nil {
		req = *in.Requires
	}
	if usesOsquery && req.Osctrl == nil {
		t := true
		req.Osctrl = &OsctrlReq{Required: &t}
	}
	declared := map[string]bool{}
	for _, p := range req.Plugins {
		declared[p.Name] = true
	}
	for _, name := range plugOrder {
		if declared[name] {
			continue
		}
		g := plugins[name]
		var actions []string
		for a := range g.actions {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		req.Plugins = append(req.Plugins, PluginReq{Name: name, Actions: actions, Repo: g.repo, Ref: g.ref, Subdir: g.subdir})
	}

	manifest := map[string]any{
		"$schema":     "https://venapce.inflowenger.com/schemas/operation-1.json",
		"schema":      1,
		"id":          in.ID,
		"name":        in.Name,
		"version":     in.Version,
		"description": in.Description,
		"readme":      "README.md",
		"flows":       flows,
		"requires":    req,
	}
	if len(in.Tags) > 0 {
		manifest["tags"] = in.Tags
	}
	if len(in.Scale) > 0 {
		manifest["scale"] = in.Scale
	}
	if len(in.Authors) > 0 {
		manifest["authors"] = in.Authors
	}
	if in.License != "" {
		manifest["license"] = in.License
	}
	if in.Homepage != "" {
		manifest["homepage"] = in.Homepage
	}
	if len(in.Params) > 0 {
		manifest["params"] = in.Params
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	m, perrs := ParseManifest(raw)
	errs = append(errs, perrs...)
	if m == nil {
		return nil, keyOf, errs
	}
	readme := strings.TrimSpace(in.Readme)
	if readme == "" {
		readme = readmeScaffold(m)
	}
	files["README.md"] = readme + "\n"
	return &Bundle{Manifest: m, Files: files}, keyOf, errs
}

// readmeScaffold writes the README the package must ship when the author did
// not: the mission (the description), one section per flow in step order,
// what the estate must provide, and the params to set after install. Meant
// to be edited, but complete enough to install as it is.
func readmeScaffold(m *Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## Mission\n\n", m.Name)
	if m.Description != "" {
		b.WriteString(m.Description + "\n\n")
	} else {
		b.WriteString("_Describe what this operation finds out and where it writes it._\n\n")
	}
	b.WriteString("## Flows\n\n")
	flows := append([]FlowSpec(nil), m.Flows...)
	sort.SliceStable(flows, func(i, j int) bool { return flows[i].Step < flows[j].Step })
	for _, f := range flows {
		role := f.Role
		if f.Role == "on-row" && len(f.Subject) > 0 {
			role = "on a " + strings.Join(f.Subject, " / ")
		}
		fmt.Fprintf(&b, "### Step %d — %s (`%s`, %s)\n\n", f.Step, firstNonEmpty(f.Title, f.Key), f.Key, role)
		if f.Description != "" {
			b.WriteString(f.Description + "\n")
		} else {
			b.WriteString("_What this flow does, what it reads, what it writes._\n")
		}
		var facts []string
		if f.Schedule != "" {
			facts = append(facts, "suggested schedule `"+f.Schedule+"`")
		}
		if len(f.Writes) > 0 {
			facts = append(facts, "writes "+strings.Join(f.Writes, ", "))
		}
		if len(facts) > 0 {
			b.WriteString(strings.Join(facts, " · ") + "\n")
		}
		b.WriteString("\n")
	}
	if m.Requires != nil && (m.Requires.Osctrl != nil || len(m.Requires.Plugins) > 0 || len(m.Requires.Settings) > 0) {
		b.WriteString("## What it needs\n\n")
		if m.Requires.Osctrl != nil {
			b.WriteString("- An osctrl the venapce plugin can reach, with nodes enrolled (your own, or a managed space from Settings → FloMorphic).\n")
		}
		for _, p := range m.Requires.Plugins {
			line := fmt.Sprintf("- The **%s** plugin on FloMorphic (%s)", p.Name, strings.Join(p.Actions, ", "))
			if p.Repo != "" {
				line += " — " + p.Repo
			}
			b.WriteString(line + ".\n")
		}
		for _, s := range m.Requires.Settings {
			line := "- A " + s.Kind + " settings profile"
			if s.For != "" {
				line += " for " + s.For
			}
			b.WriteString(line + ".\n")
		}
		b.WriteString("\n")
	}
	if len(m.Params) > 0 {
		b.WriteString("## After install\n\nSet the parameters:\n\n")
		names := make([]string, 0, len(m.Params))
		for n := range m.Params {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := m.Params[n]
			fmt.Fprintf(&b, "- `%s` — %s\n", n, firstNonEmpty(p.Description, p.Label, p.Type))
		}
		b.WriteString("\nthen press **Run** on the entry flow.\n")
	} else {
		b.WriteString("## After install\n\nPress **Run** on the entry flow.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Zip writes the bundle as a zip archive with one top-level folder named
// after the manifest id — the layout a catalog repository expects.
func Zip(b *Bundle) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	root := b.Manifest.ID + "/"
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		if p != ManifestFile {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	// The manifest comes from the stored document, never from files.
	w, err := zw.Create(root + ManifestFile)
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, b.Manifest.Raw, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(b.Manifest.Raw)
	}
	pretty.WriteByte('\n')
	if _, err := w.Write(pretty.Bytes()); err != nil {
		return nil, err
	}
	for _, p := range paths {
		w, err := zw.Create(root + strings.TrimLeft(p, "/"))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(b.Files[p])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
