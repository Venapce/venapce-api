package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
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

// A run that hit node errors and still completed must not read as a clean one:
// the ledger lands in `errors`, and `error` — empty, because nothing was fatal —
// gets the summary so every list and caller that reads it alone says so.
func TestLiftErrors(t *testing.T) {
	ledger := &flomorphic.RunErrors{Pid: "p1", Count: 3, Items: []flomorphic.NodeError{
		{Ts: 1, Kind: "node", Flow: "f1", Node: "n1", Src: "js", Code: 25, Msg: "boom"},
		{Ts: 2, Kind: "system", Flow: "f1", Node: "n2", Src: "rt", Code: 19, Msg: "infra silent"},
	}}

	var a db.Activity
	liftErrors(&a, ledger)
	if a.Error != "3 errors recorded during this run · 1 node, 1 system" {
		t.Errorf("error = %q, want the ledger's summary", a.Error)
	}
	if got := activityErrors(a.Errors); got == nil || got.Count != 3 || len(got.Items) != 2 {
		t.Errorf("stored ledger = %+v, want 3 counted / 2 carried", got)
	}

	// A fatal cause already on the row is the better message; the ledger does
	// not overwrite it.
	fatal := db.Activity{Error: "the engine stopped"}
	liftErrors(&fatal, ledger)
	if fatal.Error != "the engine stopped" {
		t.Errorf("error = %q, want the fatal cause kept", fatal.Error)
	}

	// A clean run stores an empty object, not null — `errors` is NOT NULL.
	clean := db.Activity{Error: ""}
	liftErrors(&clean, nil)
	if string(clean.Errors) != "{}" || clean.Error != "" {
		t.Errorf("clean run = %s / %q, want {} and no error", clean.Errors, clean.Error)
	}
	if got := activityErrors(clean.Errors); got != nil {
		t.Errorf("activityErrors({}) = %+v, want nil", got)
	}
}

// The ledger is read off the process row, and off the context header only when
// the process row has none — an engine that stamps the header into a FloMorphic
// that does not yet lift it must not lose it.
func TestProcessLedger(t *testing.T) {
	if got := (&flomorphic.Process{}).Ledger(); got != nil {
		t.Errorf("a run that recorded nothing = %+v, want nil", got)
	}
	// An empty object from an engine that predates the ledger is not an error.
	if got := (&flomorphic.Process{Errors: &flomorphic.RunErrors{}}).Ledger(); got != nil {
		t.Errorf("empty ledger = %+v, want nil", got)
	}
	// No count filled in: counted from the items, and the run's pid stamped on.
	p := &flomorphic.Process{PID: "p9", Errors: &flomorphic.RunErrors{Items: []flomorphic.NodeError{{Msg: "x"}}}}
	got := p.Ledger()
	if got == nil || got.Count != 1 || got.Pid != "p9" {
		t.Fatalf("ledger = %+v, want count 1 on pid p9", got)
	}
	// An item with no kind is the flow's own, which is what Summary counts.
	if want := "1 error recorded during this run · 1 node"; got.Summary() != want {
		t.Errorf("summary = %q, want %q", got.Summary(), want)
	}

	header := map[string]any{"_errors": map[string]any{"pid": "p9", "count": 2, "items": []any{
		map[string]any{"kind": "system", "msg": "infra silent"},
	}}}
	from := flomorphic.LedgerFromHeader(header)
	if from == nil || from.Count != 2 || len(from.Items) != 1 {
		t.Errorf("header ledger = %+v, want 2 counted / 1 carried", from)
	}
	if flomorphic.LedgerFromHeader(map[string]any{}) != nil {
		t.Error("a header with no _errors should yield no ledger")
	}
}
