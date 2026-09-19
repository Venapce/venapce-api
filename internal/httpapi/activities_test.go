package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/Venapce/venapce-api/internal/db"
)

// A flow may leave facts as the canonical list or as a plain object; both
// must land as [{k,v}] with v kept as whatever JSON it was.
func TestNormalizeFacts(t *testing.T) {
	cases := map[string]string{
		`[{"k":"severity","v":"low"},{"k":"score","v":7.5}]`: `[{"k":"severity","v":"low"},{"k":"score","v":7.5}]`,
		`{"severity":"low","cve":["CVE-1","CVE-2"]}`:          `[{"k":"cve","v":["CVE-1","CVE-2"]},{"k":"severity","v":"low"}]`,
		`[{"k":"","v":1},{"v":2},{"k":"ok"}]`:                  `[{"k":"ok","v":null}]`,
		`"nope"`: `[]`,
		``:       `[]`,
		`{bad`:   `[]`,
	}
	for in, want := range cases {
		if got := string(normalizeFacts(json.RawMessage(in))); got != want {
			t.Errorf("normalizeFacts(%s) = %s, want %s", in, got, want)
		}
	}
}

// The run's final context document maps onto the typed columns; keys the flow
// did not set leave the column alone, and `data` keeps the document minus the
// launch-time input.
func TestLiftOutcome(t *testing.T) {
	a := db.Activity{Title: "CVE check", Description: "kept", Facts: json.RawMessage("[]")}
	doc := map[string]any{
		"subject":  map[string]any{"kind": "stage", "id": 1},
		"row":      map[string]any{"big": true},
		"history":  []any{},
		"activity": map[string]any{"id": 5},
		"scratch":  "left by the flow",
		"outcome": map[string]any{
			"remediation": "apt upgrade openssl",
			"proof":       []any{map[string]any{"cve": "CVE-2024-1"}},
			"facts":       map[string]any{"severity": "high"},
			"tags":        []any{"cve", " openssl "},
		},
	}
	liftOutcome(&a, doc)
	if a.Title != "CVE check" || a.Description != "kept" {
		t.Errorf("unset outcome keys must not clear columns: %+v", a)
	}
	if a.Remediation != "apt upgrade openssl" {
		t.Errorf("remediation = %q", a.Remediation)
	}
	if a.Proof != `[{"cve":"CVE-2024-1"}]` {
		t.Errorf("non-string proof should be compact JSON, got %q", a.Proof)
	}
	if string(a.Facts) != `[{"k":"severity","v":"high"}]` {
		t.Errorf("facts = %s", a.Facts)
	}
	if len(a.Tags) != 2 || a.Tags[1] != "openssl" {
		t.Errorf("tags = %v", a.Tags)
	}
	var data map[string]any
	if err := json.Unmarshal(a.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"row", "history", "subject", "activity"} {
		if _, bad := data[k]; bad {
			t.Errorf("data must not carry launch input %q", k)
		}
	}
	if data["scratch"] != "left by the flow" || data["outcome"] == nil {
		t.Errorf("data should keep the flow's own keys, got %v", data)
	}

	// An explicit outcome.data wins over the trimmed document.
	b := db.Activity{}
	liftOutcome(&b, map[string]any{"outcome": map[string]any{"data": []any{1, 2}}, "scratch": 1})
	if string(b.Data) != `[1,2]` {
		t.Errorf("explicit outcome.data should be kept as-is, got %s", b.Data)
	}
}
