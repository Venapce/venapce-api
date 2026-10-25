package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/version"
)

// Preferences export / import: the configuration an operator BUILDS, carried
// between installs as one JSON file.
//
// What a firm assembles by hand — the charts it built, the dashboards it
// arranged them on and the saved views in its sidebar — is the expensive part
// of setting Venapce up, and until now the only way to move it was to redo it.
// This is the file that moves it: a demo install's work becomes the starting
// point of the real one, and a staging install can be made to match production
// without a database dump.
//
// WHAT IS DELIBERATELY NOT IN IT
//
//   - stage rows, findings, issues and activities — the DATA. A preferences
//     file is meant to be mailed, committed to a repository and diffed, and
//     rows about real entities are none of those things. Moving data is a
//     database job with a data-protection question attached; moving
//     preferences is not, and keeping them apart is what lets this endpoint be
//     one button.
//   - secrets. The Superset, osctrl and FloMorphic connections live in
//     `settings` with an AES-GCM blob keyed by this appliance's
//     APP_SECRET_KEY, so exporting them would either leak a credential or ship
//     a ciphertext the next install cannot read. Connections are configured per
//     install, by hand.
//   - operations and the flows they install. A package is already portable on
//     its own terms (export it from the operation page and import it there),
//     and its flows live on FloMorphic rather than here.
//
// IDENTITY ACROSS INSTALLS. Nothing is matched on a serial id, because the id
// of a chart here says nothing about the chart there. Each section is matched
// on what names it to a person:
//
//	charts        title
//	dashboards    slug, or title when the slug is empty
//	views         name + table
//
// Dashboards are the one section that cannot be self-contained: a cell points
// at a chart by id. So an exported chart carries its source id as `ref`, the
// layout keeps those source ids, and the import rewrites each cell through the
// map it just built. A cell whose chart is not in the file is dropped and
// reported rather than left pointing at whatever chart happens to hold that id
// here — which would silently put someone else's chart on the dashboard.

const (
	// prefsKind marks the file, so a wrong file chosen in a file picker is
	// refused with a sentence rather than half-applied.
	prefsKind = "venapce.preferences"
	// prefsVersion is the bundle's shape. Read leniently (anything at or below
	// this is accepted), refused above: a file written by a newer build may
	// carry a section this one would silently drop.
	prefsVersion = 1
)

// prefsBundle is the file. Every section is optional — an export of the charts
// alone is a valid bundle — and a section that was not asked for is absent
// rather than empty, so "export everything" and "export nothing of this" stay
// distinguishable on the way back in.
type prefsBundle struct {
	Kind       string    `json:"kind"`
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`
	// AppVersion is informational: which build wrote the file.
	AppVersion string `json:"appVersion,omitempty"`
	// Label is the operator's own note, carried through untouched.
	Label string `json:"label,omitempty"`

	Charts     []prefChart     `json:"charts,omitempty"`
	Dashboards []prefDashboard `json:"dashboards,omitempty"`
	Views      []prefView      `json:"views,omitempty"`
}

type prefChart struct {
	// Ref is the chart's id in the install that wrote the file — the handle a
	// dashboard layout points at, and meaningless anywhere else.
	Ref          int64           `json:"ref,omitempty"`
	Title        string          `json:"title"`
	VizType      string          `json:"vizType"`
	QueryContext json.RawMessage `json:"queryContext,omitempty"`
	BuilderState json.RawMessage `json:"builderState,omitempty"`
}

type prefDashboard struct {
	Title  string          `json:"title"`
	Slug   string          `json:"slug,omitempty"`
	Layout json.RawMessage `json:"layout,omitempty"`
}

type prefView struct {
	Name  string   `json:"name"`
	Table string   `json:"table"`
	Tags  []string `json:"tags"`
	Match string   `json:"match,omitempty"`
}

// ---- sections ---------------------------------------------------------------

// prefsSections is every section, in APPLY ORDER. Order is a dependency, not a
// preference: a dashboard cell points at a chart, so it has to land after what
// it points at.
var prefsSections = []string{"charts", "dashboards", "views"}

// prefsImportMode decides what happens to something that is already here.
//
//	merge  update it from the file — the file wins
//	add    leave it alone and count it skipped — the install wins
//
// Nothing is ever deleted by an import. A "replace everything" mode would be
// the one button in this product that can throw away a year of an operator's
// configuration because a file was short, and the recovery for it is a
// database restore, so it is not offered.
const (
	prefsModeMerge = "merge"
	prefsModeAdd   = "add"
)

type prefsSectionReport struct {
	Name    string `json:"name"`
	Created int    `json:"created"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
}

// prefsReport is what the import answers, and what a dry run answers without
// writing: per-section counts plus the things that could not be applied.
type prefsReport struct {
	DryRun   bool                 `json:"dryRun"`
	Mode     string               `json:"mode"`
	Sections []prefsSectionReport `json:"sections"`
	Created  int                  `json:"created"`
	Updated  int                  `json:"updated"`
	Skipped  int                  `json:"skipped"`
	// Notes are the per-item problems that did not stop the import: a view
	// with no tags, a dashboard cell whose chart was not in the file.
	Notes []string `json:"notes"`
}

func (r *prefsReport) section(name string) *prefsSectionReport {
	r.Sections = append(r.Sections, prefsSectionReport{Name: name})
	return &r.Sections[len(r.Sections)-1]
}

func (r *prefsReport) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// total sums the per-section counts once every section has run.
func (r *prefsReport) total() {
	r.Created, r.Updated, r.Skipped = 0, 0, 0
	for _, s := range r.Sections {
		r.Created += s.Created
		r.Updated += s.Updated
		r.Skipped += s.Skipped
	}
	if r.Notes == nil {
		r.Notes = []string{}
	}
}

// ---- export -----------------------------------------------------------------

// GET /api/settings/preferences?include=charts,views
//
// Without `include` every section is exported. The panel sends the list so an
// operator can take the saved views without the charts, which is the common
// case when the two installs chart different warehouses.
func (s *Server) exportPreferences(c fiber.Ctx) error {
	want, err := prefsSelection(splitCSV(c.Query("include")), nil)
	if err != nil {
		return err
	}
	selected := make(map[string]bool, len(want))
	for _, name := range want {
		selected[name] = true
	}

	out := prefsBundle{
		Kind:       prefsKind,
		Version:    prefsVersion,
		ExportedAt: time.Now().UTC(),
		AppVersion: version.Current(),
		Label:      strings.TrimSpace(c.Query("label")),
	}

	if selected["charts"] {
		rows, err := s.q.ListCharts(c.Context())
		if err != nil {
			return err
		}
		out.Charts = make([]prefChart, 0, len(rows))
		for _, ch := range rows {
			out.Charts = append(out.Charts, prefChart{
				Ref:          ch.ID,
				Title:        ch.Title,
				VizType:      ch.VizType,
				QueryContext: rawOr(ch.QueryContext, "{}"),
				BuilderState: rawOr(ch.BuilderState, "{}"),
			})
		}
	}

	if selected["dashboards"] {
		rows, err := s.q.ListDashboards(c.Context())
		if err != nil {
			return err
		}
		out.Dashboards = make([]prefDashboard, 0, len(rows))
		for _, d := range rows {
			out.Dashboards = append(out.Dashboards, prefDashboard{
				Title:  d.Title,
				Slug:   d.Slug,
				Layout: rawOr(d.Layout, "[]"),
			})
		}
	}

	if selected["views"] {
		rows, err := s.q.ListViews(c.Context(), "")
		if err != nil {
			return err
		}
		out.Views = make([]prefView, 0, len(rows))
		for _, v := range rows {
			out.Views = append(out.Views, prefView{
				Name:  v.Name,
				Table: v.Target,
				Tags:  orEmpty(v.Tags),
				Match: v.MatchMode,
			})
		}
	}

	return c.JSON(out)
}

// ---- import -----------------------------------------------------------------

type prefsImportBody struct {
	Bundle *prefsBundle `json:"bundle"`
	// Include narrows what is applied. Absent means every section the file
	// carries.
	Include []string `json:"include"`
	Mode    string   `json:"mode"`
	// DryRun applies everything inside a transaction and rolls it back, so the
	// report is the real one — same conflicts, same skips — with nothing kept.
	DryRun bool `json:"dryRun"`
}

// POST /api/settings/preferences/import
//
// The whole import is one transaction: either the file applied or nothing did.
// A set of dashboards whose charts did not arrive is a worse state than the one
// before the import, and an operator cannot tell by looking which half landed.
func (s *Server) importPreferences(c fiber.Ctx) error {
	var body prefsImportBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validatePrefsBundle(body.Bundle); err != nil {
		return err
	}
	mode := prefsMode(body.Mode)
	want, err := prefsSelection(body.Include, body.Bundle)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return fiber.NewError(fiber.StatusBadRequest,
			"this file carries none of the sections an import can apply (charts, dashboards, views)")
	}

	tx, err := s.pool.Begin(c.Context())
	if err != nil {
		return err
	}
	// Rolled back on every path that does not explicitly commit, the dry run
	// included. A Rollback after a successful Commit is a no-op.
	defer tx.Rollback(c.Context())
	q := s.q.WithTx(tx)

	report := prefsReport{DryRun: body.DryRun, Mode: mode}
	// Carried between sections: the source-id → local-id map a dashboard
	// layout is rewritten through.
	charts := map[int64]int64{}

	for _, name := range want {
		switch name {
		case "charts":
			err = s.importCharts(c, q, body.Bundle.Charts, mode, &report, charts)
		case "dashboards":
			err = s.importDashboards(c, q, body.Bundle.Dashboards, body.Bundle.Charts, mode, &report, charts)
		case "views":
			err = s.importViews(c, q, body.Bundle.Views, mode, &report)
		}
		if err != nil {
			return err
		}
	}
	report.total()

	if body.DryRun {
		return c.JSON(report)
	}
	if err := tx.Commit(c.Context()); err != nil {
		return err
	}
	return c.JSON(report)
}

// importCharts applies the saved charts, matched on title, and records for each
// one the source id → local id a dashboard layout is rewritten through.
func (s *Server) importCharts(c fiber.Ctx, q *db.Queries, charts []prefChart, mode string, rep *prefsReport, refs map[int64]int64) error {
	sec := rep.section("charts")
	if len(charts) == 0 {
		return nil
	}
	rows, err := q.ListCharts(c.Context())
	if err != nil {
		return err
	}
	existing := map[string]int64{}
	for _, ch := range rows {
		if _, seen := existing[ch.Title]; !seen {
			existing[ch.Title] = ch.ID
		}
	}

	for _, ch := range charts {
		title := strings.TrimSpace(ch.Title)
		if title == "" {
			rep.note("charts: a chart with no title was skipped")
			sec.Skipped++
			continue
		}
		vizType := orDefault(strings.TrimSpace(ch.VizType), "bar")
		if id, exists := existing[title]; exists {
			if mode == prefsModeAdd {
				// Still mapped: a dashboard in the file should land on the
				// chart of that name here rather than lose the cell.
				if ch.Ref != 0 {
					refs[ch.Ref] = id
				}
				sec.Skipped++
				continue
			}
			updated, err := q.UpdateChart(c.Context(), db.UpdateChartParams{
				ID:           id,
				Title:        title,
				VizType:      vizType,
				QueryContext: rawOr(ch.QueryContext, "{}"),
				BuilderState: rawOr(ch.BuilderState, "{}"),
			})
			if err != nil {
				return err
			}
			if ch.Ref != 0 {
				refs[ch.Ref] = updated.ID
			}
			sec.Updated++
			continue
		}
		created, err := q.CreateChart(c.Context(), db.CreateChartParams{
			Title:        title,
			VizType:      vizType,
			QueryContext: rawOr(ch.QueryContext, "{}"),
			BuilderState: rawOr(ch.BuilderState, "{}"),
		})
		if err != nil {
			return err
		}
		existing[title] = created.ID
		if ch.Ref != 0 {
			refs[ch.Ref] = created.ID
		}
		sec.Created++
	}
	return nil
}

// importDashboards applies the grid arrangements, rewriting every cell through
// the chart map. A dashboard left with no cells it can resolve is still
// created — an empty dashboard with the right name is editable, while a
// silently absent one looks like the import failed.
func (s *Server) importDashboards(c fiber.Ctx, q *db.Queries, boards []prefDashboard, charts []prefChart, mode string, rep *prefsReport, refs map[int64]int64) error {
	sec := rep.section("dashboards")
	if len(boards) == 0 {
		return nil
	}
	// The dashboards were selected but the charts were not — the operator
	// wants this install's charts under the file's arrangement. The file still
	// says which chart each cell meant, by title, so the cells that have a
	// namesake here are placed instead of dropped.
	if err := fillChartRefsByTitle(c, q, charts, refs); err != nil {
		return err
	}
	rows, err := q.ListDashboards(c.Context())
	if err != nil {
		return err
	}
	bySlug := map[string]db.Dashboard{}
	byTitle := map[string]db.Dashboard{}
	for _, d := range rows {
		if d.Slug != "" {
			if _, seen := bySlug[d.Slug]; !seen {
				bySlug[d.Slug] = d
			}
		}
		if _, seen := byTitle[d.Title]; !seen {
			byTitle[d.Title] = d
		}
	}

	for _, d := range boards {
		title := strings.TrimSpace(d.Title)
		slug := strings.TrimSpace(d.Slug)
		if title == "" && slug == "" {
			rep.note("dashboards: an entry with neither title nor slug was skipped")
			sec.Skipped++
			continue
		}
		layout, dropped := remapLayout(d.Layout, refs)
		if dropped > 0 {
			// Accurate about WHY: the chart may well be in the file and simply
			// not selected, or named differently here. Either way the cell had
			// nothing here to point at.
			rep.note("dashboards: %s lost %d cell(s) — their charts are not in this install (import the charts too, or create them under the same titles)",
				orDefault(title, slug), dropped)
		}

		cur, exists := db.Dashboard{}, false
		if slug != "" {
			cur, exists = bySlug[slug]
		}
		if !exists {
			cur, exists = byTitle[title]
		}
		if exists {
			if mode == prefsModeAdd {
				sec.Skipped++
				continue
			}
			if _, err := q.UpdateDashboard(c.Context(), db.UpdateDashboardParams{
				ID:    cur.ID,
				Title: orDefault(title, cur.Title),
				// A file with no slug keeps the one this install gave the
				// dashboard: the slug is this install's own link to it, and
				// blanking it would break whatever points at it.
				Slug:   orDefault(slug, cur.Slug),
				Layout: layout,
			}); err != nil {
				return err
			}
			sec.Updated++
			continue
		}
		created, err := q.CreateDashboard(c.Context(), db.CreateDashboardParams{
			Title:  orDefault(title, slug),
			Slug:   slug,
			Layout: layout,
		})
		if err != nil {
			return err
		}
		if created.Slug != "" {
			bySlug[created.Slug] = created
		}
		byTitle[created.Title] = created
		sec.Created++
	}
	return nil
}

// importViews applies the saved tag filters. The tags are not checked against
// anything: a view over a tag no row carries yet is harmless — it simply
// matches nothing until such a row arrives — and refusing it would make the
// order of an import matter in a way an operator cannot see.
func (s *Server) importViews(c fiber.Ctx, q *db.Queries, views []prefView, mode string, rep *prefsReport) error {
	sec := rep.section("views")
	if len(views) == 0 {
		return nil
	}
	rows, err := q.ListViews(c.Context(), "")
	if err != nil {
		return err
	}
	existing := map[string]db.View{}
	for _, v := range rows {
		k := v.Name + "\x00" + v.Target
		if _, seen := existing[k]; !seen {
			existing[k] = v
		}
	}

	for _, v := range views {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			rep.note("views: a view with no name was skipped")
			sec.Skipped++
			continue
		}
		target := orDefault(strings.TrimSpace(v.Table), "issues")
		match := orDefault(strings.TrimSpace(v.Match), "any")
		if err := validateView(target, match); err != nil {
			rep.note("views: %s — %v, skipped", name, err)
			sec.Skipped++
			continue
		}
		tags := orEmpty(v.Tags)
		if len(tags) == 0 {
			rep.note("views: %s carries no tags, and a view is a tag filter — skipped", name)
			sec.Skipped++
			continue
		}
		if cur, exists := existing[name+"\x00"+target]; exists {
			if mode == prefsModeAdd {
				sec.Skipped++
				continue
			}
			if _, err := q.UpdateView(c.Context(), db.UpdateViewParams{
				ID: cur.ID, Name: name, Target: target, Tags: tags, MatchMode: match,
			}); err != nil {
				return err
			}
			sec.Updated++
			continue
		}
		created, err := q.CreateView(c.Context(), db.CreateViewParams{
			Name: name, Target: target, Tags: tags, MatchMode: match,
		})
		if err != nil {
			return err
		}
		existing[name+"\x00"+target] = created
		sec.Created++
	}
	return nil
}

// ---- pure helpers -----------------------------------------------------------

// validatePrefsBundle checks the envelope. Both refusals are about telling an
// operator what they handed over: the wrong file entirely, or one from a build
// that knows sections this one would drop without saying so.
func validatePrefsBundle(b *prefsBundle) error {
	if b == nil {
		return fiber.NewError(fiber.StatusBadRequest, "no bundle in the request")
	}
	if b.Kind != "" && b.Kind != prefsKind {
		return fiber.NewError(fiber.StatusBadRequest,
			"this is not a Venapce preferences file (kind is "+b.Kind+")")
	}
	if b.Version > prefsVersion {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf(
			"this file was written by a newer Venapce (format %d, this build reads %d) — upgrade this install first",
			b.Version, prefsVersion))
	}
	return nil
}

// prefsSelection resolves what to act on: the requested sections, in apply
// order, narrowed to those the bundle actually carries when one is given.
// An unknown name is refused rather than ignored, because a typo in `include`
// otherwise reads as "that section was empty".
func prefsSelection(requested []string, b *prefsBundle) ([]string, error) {
	known := map[string]bool{}
	for _, name := range prefsSections {
		known[name] = true
	}
	want := map[string]bool{}
	for _, raw := range requested {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fiber.NewError(fiber.StatusBadRequest, "unknown section: "+raw+
				" (known: "+strings.Join(prefsSections, ", ")+")")
		}
		want[name] = true
	}
	all := len(want) == 0 // nothing named means everything

	out := make([]string, 0, len(prefsSections))
	for _, name := range prefsSections {
		if !all && !want[name] {
			continue
		}
		if b != nil && !bundleHas(b, name) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// bundleHas reports whether the file carries a section at all, so an import
// does not claim to have applied one that was never in it.
func bundleHas(b *prefsBundle, name string) bool {
	switch name {
	case "charts":
		return b.Charts != nil
	case "dashboards":
		return b.Dashboards != nil
	case "views":
		return b.Views != nil
	}
	return false
}

func prefsMode(raw string) string {
	if strings.TrimSpace(strings.ToLower(raw)) == prefsModeAdd {
		return prefsModeAdd
	}
	return prefsModeMerge
}

// remapLayout rewrites a dashboard's cells through the source-id → local-id
// chart map, and says how many cells it had to drop. A cell whose chart is not
// in the map is dropped rather than carried through: the id would resolve here
// to whatever chart happens to hold it, which puts a stranger's chart on
// someone's dashboard and looks for all the world like it belongs there.
func remapLayout(layout json.RawMessage, refs map[int64]int64) (json.RawMessage, int) {
	if len(layout) == 0 {
		return json.RawMessage("[]"), 0
	}
	var cells []map[string]any
	if err := json.Unmarshal(layout, &cells); err != nil {
		return json.RawMessage("[]"), 0
	}
	out := make([]map[string]any, 0, len(cells))
	dropped := 0
	for _, cell := range cells {
		raw, ok := cell["chartId"]
		if !ok {
			// Not a chart cell (a future text or spacer tile): carried through
			// untouched rather than thrown away.
			out = append(out, cell)
			continue
		}
		num, ok := raw.(float64)
		if !ok {
			dropped++
			continue
		}
		local, ok := refs[int64(num)]
		if !ok {
			dropped++
			continue
		}
		cell["chartId"] = local
		out = append(out, cell)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage("[]"), dropped
	}
	return b, dropped
}

// fillChartRefsByTitle resolves the file's chart references against the charts
// ALREADY here, for the refs the charts section did not fill — which is every
// one of them when the charts were left out of the import.
//
// Title is the only handle a layout cell leaves behind, and it is the same
// identity the charts section matches on, so a dashboard imported on its own
// lands on the charts an operator would expect it to.
func fillChartRefsByTitle(c fiber.Ctx, q *db.Queries, charts []prefChart, refs map[int64]int64) error {
	want := map[string]int64{} // title -> source ref still unresolved
	for _, ch := range charts {
		title := strings.TrimSpace(ch.Title)
		if ch.Ref == 0 || title == "" {
			continue
		}
		if _, done := refs[ch.Ref]; done {
			continue
		}
		if _, seen := want[title]; !seen {
			want[title] = ch.Ref
		}
	}
	if len(want) == 0 {
		return nil
	}
	rows, err := q.ListCharts(c.Context())
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ref, ok := want[row.Title]; ok {
			if _, done := refs[ref]; !done {
				refs[ref] = row.ID
			}
		}
	}
	return nil
}
