package httpapi

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// Helpers shared by the stage / findings / issues handlers. The three tables
// speak one vocabulary (source, origin, ref, data, meta, tags, typed *_id
// links), and their update endpoints all take a PARTIAL body: only the fields
// present in the JSON change, everything else keeps its stored value. The
// pointer-typed bodies below make "absent" distinguishable from "empty".

// splitTags turns a comma-separated tag list ("untrusted,network") into a slice,
// trimming blanks. Always returns a non-nil slice (sqlc array params want one).
func splitTags(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// pick returns the body value when the field was present, else the current one.
func pick(cur string, v *string) string {
	if v == nil {
		return cur
	}
	return *v
}

func pickInt(cur int64, v *int64) int64 {
	if v == nil {
		return cur
	}
	return *v
}

func pickTags(cur []string, v *[]string) []string {
	if v == nil {
		return orEmpty(cur)
	}
	return orEmpty(*v)
}

// pickJSON keeps the stored document unless the body carried one. A JSON `null`
// (or a non-object/array scalar) is accepted as-is — the columns are free-form.
func pickJSON(cur json.RawMessage, v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return rawOr(cur, "{}")
	}
	if !json.Valid(v) {
		return rawOr(cur, "{}")
	}
	return v
}

// validJSONFields rejects a body whose free-form documents are not valid JSON,
// naming the field so the editor can point at it.
func validJSONFields(fields map[string]json.RawMessage) error {
	for name, raw := range fields {
		if len(raw) > 0 && !json.Valid(raw) {
			return fiber.NewError(fiber.StatusBadRequest, name+" must be valid JSON")
		}
	}
	return nil
}

// promoteTarget reads which table a row is promoted into: `to` in the query or
// the body ("issue" by default; "finding" is the intermediate level).
func promoteTarget(c fiber.Ctx, bodyTo string) (string, error) {
	to := orDefault(c.Query("to"), orDefault(bodyTo, "issue"))
	switch to {
	case "issue", "finding":
		return to, nil
	}
	return "", fiber.NewError(fiber.StatusBadRequest, "to must be 'finding' or 'issue'")
}
