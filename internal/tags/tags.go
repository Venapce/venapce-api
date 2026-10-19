// Package tags normalizes flow labels — the one place Venapce agrees with
// FloMorphic on what a tag is.
//
// Flow tags are how one operation's flows are told apart from everything else
// on a shared FloMorphic canvas: an operation stamps its labels on the flows it
// installs, and the run picker asks FloMorphic for the flows carrying them
// (GET /flow?tags=). That filter is a membership test over a comma-delimited
// string on the FloMorphic side, so the rules here mirror its
// models.NormalizeTags exactly — same casing, same de-duplication, no commas.
// A tag that normalized differently on the two sides would simply never match.
package tags

import "strings"

// Limits, matching FloMorphic's models.MaxTagLen / MaxTagCount.
const (
	MaxLen   = 64
	MaxCount = 32
)

// Clean canonicalises a tag list: trimmed, lower-cased, inner whitespace
// collapsed to a dash, commas and quotes dropped, empties removed, duplicates
// removed in first-seen order. It always returns a non-nil slice (possibly
// empty), so a caller can range over it without a nil check.
func Clean(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		t := strings.ToLower(strings.TrimSpace(raw))
		t = strings.Map(func(r rune) rune {
			switch r {
			case ',', '"':
				return -1
			case ' ', '\t', '\n', '\r':
				return '-'
			}
			return r
		}, t)
		for strings.Contains(t, "--") {
			t = strings.ReplaceAll(t, "--", "-")
		}
		t = strings.Trim(t, "-")
		if t == "" || seen[t] {
			continue
		}
		if len(t) > MaxLen {
			t = t[:MaxLen]
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == MaxCount {
			break
		}
	}
	return out
}

// Split reads a comma-separated tag parameter (a `?tags=` query string) into a
// cleaned list.
func Split(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return Clean(strings.Split(raw, ","))
}

// Union cleans and merges several tag lists into one, order preserved. It is
// what turns "the package's flow tags" plus "this flow's own tags" into the set
// stamped on an installed flow.
func Union(lists ...[]string) []string {
	var all []string
	for _, l := range lists {
		all = append(all, l...)
	}
	return Clean(all)
}
