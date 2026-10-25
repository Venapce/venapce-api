package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/Venapce/venapce-api/internal/config"
)

// A preferences file is chosen in a file picker, so the two things worth
// refusing are the wrong file entirely and one from a build that knows more
// sections than this one — which would otherwise be half-applied in silence.
func TestValidatePrefsBundle(t *testing.T) {
	cases := []struct {
		name string
		in   *prefsBundle
		want int // 0 = accepted
	}{
		{"nothing sent", nil, fiber.StatusBadRequest},
		{"a venapce preferences file", &prefsBundle{Kind: prefsKind, Version: prefsVersion}, 0},
		{"some other json", &prefsBundle{Kind: "venapce.operation", Version: 1}, fiber.StatusBadRequest},
		// Hand-written or trimmed files are let through: the sections are what
		// matters, and refusing a missing envelope would make the format
		// hostile to the operator editing it in an editor.
		{"no envelope at all", &prefsBundle{}, 0},
		{"from a newer build", &prefsBundle{Kind: prefsKind, Version: prefsVersion + 1}, fiber.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePrefsBundle(tc.in)
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want %d, got accepted", tc.want)
			}
			fe, _ := err.(*fiber.Error)
			if fe == nil || fe.Code != tc.want {
				t.Fatalf("want %d, got %v", tc.want, err)
			}
		})
	}
}

// The selection decides what an import touches, so the two rules it carries are
// the ones a wrong answer would hide: sections come back in APPLY order (a
// dashboard needs its charts first), and a section the file does not carry is
// dropped rather than reported as empty.
func TestPrefsSelection(t *testing.T) {
	bundle := &prefsBundle{
		Kind:    prefsKind,
		Version: prefsVersion,
		Charts:  []prefChart{{Title: "Issues by severity"}},
		Views:   []prefView{{Name: "Unowned", Table: "issues", Tags: []string{"owner.none"}}},
	}

	got, err := prefsSelection(nil, bundle)
	if err != nil {
		t.Fatalf("select everything: %v", err)
	}
	if len(got) != 2 || got[0] != "charts" || got[1] != "views" {
		t.Fatalf("want [charts views] (apply order, bundle-narrowed), got %v", got)
	}

	// Named in the wrong order, answered in apply order.
	got, err = prefsSelection([]string{"views", "charts"}, bundle)
	if err != nil {
		t.Fatalf("named sections: %v", err)
	}
	if len(got) != 2 || got[0] != "charts" || got[1] != "views" {
		t.Fatalf("want apply order regardless of the request order, got %v", got)
	}

	// Asked for a section this file has none of.
	got, err = prefsSelection([]string{"dashboards"}, bundle)
	if err != nil {
		t.Fatalf("absent section: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want nothing to apply, got %v", got)
	}

	// An export narrows nothing — there is no bundle to narrow against.
	got, err = prefsSelection(nil, nil)
	if err != nil {
		t.Fatalf("export selection: %v", err)
	}
	if len(got) != len(prefsSections) {
		t.Fatalf("want every section for an export, got %v", got)
	}

	// A typo must not read as "that section was empty".
	if _, err := prefsSelection([]string{"charts", "dashbords"}, bundle); err == nil {
		t.Fatal("want an unknown section to be refused")
	}
}

// An empty section and an absent one are different answers: "export the charts
// and nothing else" has to stay distinguishable from "this install has no
// charts", or an import of the first would claim to have applied the rest.
func TestBundleHasDistinguishesEmptyFromAbsent(t *testing.T) {
	absent := &prefsBundle{}
	if bundleHas(absent, "charts") {
		t.Fatal("an absent section must not count as carried")
	}
	empty := &prefsBundle{Charts: []prefChart{}}
	if !bundleHas(empty, "charts") {
		t.Fatal("an empty section was exported deliberately and must count as carried")
	}
}

// A dashboard cell points at a chart by id, and an id means nothing across
// installs. The cell that cannot be resolved is dropped rather than carried:
// here that id belongs to whatever chart happens to hold it, which would put a
// stranger's chart on someone's dashboard and look deliberate.
func TestRemapLayout(t *testing.T) {
	refs := map[int64]int64{7: 101, 8: 102}

	layout := json.RawMessage(`[
      {"chartId": 7, "x": 0, "y": 0, "w": 6, "h": 8},
      {"chartId": 8, "x": 6, "y": 0, "w": 6, "h": 8},
      {"chartId": 99, "x": 0, "y": 8, "w": 6, "h": 8}
    ]`)
	out, dropped := remapLayout(layout, refs)
	if dropped != 1 {
		t.Fatalf("want the unresolvable cell dropped, got dropped=%d", dropped)
	}
	var cells []map[string]any
	if err := json.Unmarshal(out, &cells); err != nil {
		t.Fatalf("layout did not survive: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("want 2 cells, got %d: %s", len(cells), out)
	}
	if cells[0]["chartId"].(float64) != 101 || cells[1]["chartId"].(float64) != 102 {
		t.Fatalf("cells were not remapped: %s", out)
	}
	// The geometry is untouched — only the reference moves.
	if cells[0]["w"].(float64) != 6 || cells[1]["x"].(float64) != 6 {
		t.Fatalf("geometry changed: %s", out)
	}

	// A cell with no chart reference is a future tile, not a broken one.
	out, dropped = remapLayout(json.RawMessage(`[{"kind":"text","body":"hello"}]`), refs)
	if dropped != 0 {
		t.Fatalf("a cell with no chartId must be carried through, got dropped=%d", dropped)
	}
	if err := json.Unmarshal(out, &cells); err != nil || len(cells) != 1 {
		t.Fatalf("want the tile kept, got %s", out)
	}

	// Nothing readable: an empty grid, which is editable, rather than an error
	// that costs the whole import.
	for _, bad := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`{}`), json.RawMessage(`"nope"`)} {
		out, dropped = remapLayout(bad, refs)
		if string(out) != "[]" || dropped != 0 {
			t.Fatalf("want an empty layout for %q, got %s", bad, out)
		}
	}
}

// Merge is the default: a file an operator picked is meant to be applied, and
// anything unrecognised had better not quietly become "change nothing".
func TestPrefsMode(t *testing.T) {
	for _, in := range []string{"", "merge", "MERGE", " merge ", "nonsense"} {
		if got := prefsMode(in); got != prefsModeMerge {
			t.Fatalf("prefsMode(%q) = %q, want merge", in, got)
		}
	}
	for _, in := range []string{"add", "ADD", " add "} {
		if got := prefsMode(in); got != prefsModeAdd {
			t.Fatalf("prefsMode(%q) = %q, want add", in, got)
		}
	}
}

// The Settings panel is the only caller, so a route that silently went missing
// would show up as a dead button rather than as a failure anywhere near here.
func TestPreferencesRoutesAreRegistered(t *testing.T) {
	app := (&Server{cfg: config.Config{CORSOrigins: "http://localhost:5173"}}).App()

	cases := []struct{ method, path string }{
		{"GET", "/api/settings/preferences"},
		{"POST", "/api/settings/preferences/import"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			res, err := app.Test(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode == fiber.StatusNotFound {
				t.Fatal("route is not registered")
			}
		})
	}
}
