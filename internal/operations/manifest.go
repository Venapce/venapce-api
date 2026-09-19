// Package operations is the package manager behind /api/operations: reading
// an operation package (operation.json + the files it names) from a URL or an
// uploaded bundle, validating the manifest, and substituting the operator's
// params into the flow files before they go to FloMorphic.
//
// The manifest format is defined once, in the wapp's
// public/schemas/venapce-operation.schema.json; the Go side reads the fields
// it acts on and keeps the rest as-is, so a newer manifest with extra keys
// still installs.
package operations

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	ManifestFile = "operation.json"
	CatalogFile  = "catalog.json"
)

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
)

// ParamSpec is one operator knob of the package.
type ParamSpec struct {
	Type        string          `json:"type"`
	Label       string          `json:"label,omitempty"`
	Description string          `json:"description,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Enum        []any           `json:"enum,omitempty"`
	Secret      bool            `json:"secret,omitempty"`
}

// FlowSpec is one workflow of the package.
type FlowSpec struct {
	Key         string   `json:"key"`
	File        string   `json:"file"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Role        string   `json:"role"`
	Step        int      `json:"step,omitempty"`
	Doc         string   `json:"doc,omitempty"`
	Subject     []string `json:"subject,omitempty"`
	Schedule    string   `json:"schedule,omitempty"`
	Writes      []string `json:"writes,omitempty"`
}

type PluginReq struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
	Repo    string   `json:"repo,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Subdir  string   `json:"subdir,omitempty"`
	Note    string   `json:"note,omitempty"`
}

type OsctrlReq struct {
	Required *bool    `json:"required,omitempty"`
	Targets  []string `json:"targets,omitempty"`
	MinNodes int      `json:"minNodes,omitempty"`
	Note     string   `json:"note,omitempty"`
}

type SettingsReq struct {
	Kind string `json:"kind"`
	For  string `json:"for,omitempty"`
	Note string `json:"note,omitempty"`
}

type Requires struct {
	Osctrl     *OsctrlReq    `json:"osctrl,omitempty"`
	Plugins    []PluginReq   `json:"plugins,omitempty"`
	Settings   []SettingsReq `json:"settings,omitempty"`
	Operations []string      `json:"operations,omitempty"`
	Datasets   []string      `json:"datasets,omitempty"`
}

// Manifest is operation.json. Unknown keys survive a round trip through
// `Raw`, which is what gets stored and served.
type Manifest struct {
	Schema      int                  `json:"schema"`
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Readme      string               `json:"readme,omitempty"`
	Tags        []string             `json:"tags,omitempty"`
	Scale       []string             `json:"scale,omitempty"`
	Params      map[string]ParamSpec `json:"params,omitempty"`
	Flows       []FlowSpec           `json:"flows"`
	Requires    *Requires            `json:"requires,omitempty"`
	// The document as received, for storage.
	Raw json.RawMessage `json:"-"`
}

// ReadmePath is the package's documentation file — required to exist.
func (m *Manifest) ReadmePath() string {
	if strings.TrimSpace(m.Readme) == "" {
		return "README.md"
	}
	return m.Readme
}

// Files lists every path the manifest names, relative to its folder.
func (m *Manifest) Files() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(m.ReadmePath())
	for _, f := range m.Flows {
		add(f.File)
		add(f.Doc)
	}
	return out
}

// Flow finds a manifest flow by key.
func (m *Manifest) Flow(key string) *FlowSpec {
	for i := range m.Flows {
		if m.Flows[i].Key == key {
			return &m.Flows[i]
		}
	}
	return nil
}

// ParseManifest validates the essentials and returns every problem at once.
// The manifest is returned even with errors, so a preview can show it.
func ParseManifest(text []byte) (*Manifest, []string) {
	var m Manifest
	if err := json.Unmarshal(text, &m); err != nil {
		return nil, []string{ManifestFile + " is not valid JSON: " + err.Error()}
	}
	m.Raw = json.RawMessage(text)
	var errs []string
	if m.Schema != 1 {
		errs = append(errs, fmt.Sprintf(`"schema" must be 1 (got %d)`, m.Schema))
	}
	if !slugRe.MatchString(m.ID) {
		errs = append(errs, `"id" must be a slug (lower-case letters, digits, dashes)`)
	}
	if strings.TrimSpace(m.Name) == "" {
		errs = append(errs, `"name" is required`)
	}
	if !semverRe.MatchString(m.Version) {
		errs = append(errs, `"version" must be semver (1.2.3)`)
	}
	if len(m.Flows) == 0 {
		errs = append(errs, `"flows" must list at least one workflow`)
	}
	keys := map[string]bool{}
	for i, f := range m.Flows {
		at := fmt.Sprintf("flows[%d]", i)
		switch {
		case !slugRe.MatchString(f.Key):
			errs = append(errs, at+".key must be a slug")
		case keys[f.Key]:
			errs = append(errs, fmt.Sprintf("%s.key %q is used twice", at, f.Key))
		default:
			keys[f.Key] = true
		}
		if strings.TrimSpace(f.File) == "" {
			errs = append(errs, at+".file is required")
		}
		if f.Role != "entry" && f.Role != "on-row" && f.Role != "helper" {
			errs = append(errs, at+".role must be one of entry | on-row | helper")
		}
		if f.Role == "on-row" && len(f.Subject) == 0 {
			errs = append(errs, at+" is on-row but names no subject (stage | finding | issue)")
		}
		if f.Step < 0 {
			errs = append(errs, at+".step must be a positive integer")
		}
	}
	for name, p := range m.Params {
		switch p.Type {
		case "string", "number", "boolean", "string[]", "json":
		default:
			errs = append(errs, fmt.Sprintf("params.%s.type must be one of string | number | boolean | string[] | json", name))
		}
	}
	if m.Requires != nil {
		for _, p := range m.Requires.Plugins {
			if p.Name == "" || len(p.Actions) == 0 {
				errs = append(errs, "requires.plugins: every entry needs a name and at least one action")
			}
		}
	}
	return &m, errs
}

// Bundle is a package as stored: the manifest plus its files as text.
type Bundle struct {
	Manifest *Manifest
	Files    map[string]string
}

// Check validates the bundle's files against the manifest: the README must
// exist (it carries the mission and each flow's part in it), every flow file
// must exist and be a FloMorphic workflow export, and a named doc must exist.
func (b *Bundle) Check() []string {
	var errs []string
	m := b.Manifest
	if _, ok := b.Files[m.ReadmePath()]; !ok {
		errs = append(errs, fmt.Sprintf("Missing %q: every package must ship a README that explains the mission and each flow's part in it", m.ReadmePath()))
	}
	for _, f := range m.Flows {
		text, ok := b.Files[f.File]
		if !ok {
			errs = append(errs, fmt.Sprintf("Missing file %q named by the manifest", f.File))
			continue
		}
		var doc struct {
			Nodes      json.RawMessage `json:"nodes"`
			Flomorphic struct {
				Kind string `json:"kind"`
			} `json:"flomorphic"`
		}
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid JSON (%v)", f.File, err))
			continue
		}
		if len(doc.Nodes) == 0 || doc.Nodes[0] != '[' {
			errs = append(errs, fmt.Sprintf(`%s: not a FloMorphic workflow export (no "nodes")`, f.File))
		} else if doc.Flomorphic.Kind != "" && doc.Flomorphic.Kind != "workflow" {
			errs = append(errs, fmt.Sprintf(`%s: flomorphic.kind is %q, expected "workflow"`, f.File, doc.Flomorphic.Kind))
		}
		if f.Doc != "" {
			if _, ok := b.Files[f.Doc]; !ok {
				errs = append(errs, fmt.Sprintf("Missing file %q named by the manifest", f.Doc))
			}
		}
	}
	return errs
}

// ReferencedActions lists the plugin actions the flow files call, per flow
// key — what readiness verifies even when the manifest forgot to declare a
// plugin, and what an install reports as missing.
func (b *Bundle) ReferencedActions() map[string][]string {
	out := map[string][]string{}
	for _, f := range b.Manifest.Flows {
		text, ok := b.Files[f.File]
		if !ok {
			continue
		}
		var doc struct {
			Nodes []struct {
				Kind string `json:"kind"`
				Data struct {
					Action string `json:"action"`
				} `json:"data"`
			} `json:"nodes"`
		}
		if json.Unmarshal([]byte(text), &doc) != nil {
			continue
		}
		seen := map[string]bool{}
		for _, n := range doc.Nodes {
			a := strings.TrimSpace(n.Data.Action)
			if n.Kind == "plugin" && a != "" && !seen[a] {
				seen[a] = true
				out[f.Key] = append(out[f.Key], a)
			}
		}
		sort.Strings(out[f.Key])
	}
	return out
}

// IsVenapceAction reports whether an action is provided by the venapce plugin
// itself (db.*, osquery.*, osctrl.*).
func IsVenapceAction(action string) bool {
	return strings.HasPrefix(action, "db.") || strings.HasPrefix(action, "osquery.") || strings.HasPrefix(action, "osctrl.")
}
