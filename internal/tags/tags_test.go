package tags

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{" GDPR ", "gdpr"},
		{"ISO 27001", "iso-27001"},
		// A comma would break FloMorphic's delimited tag filter, so it cannot
		// survive normalization on either side.
		{"a,b", "ab"},
		{`say "hi"`, "say-hi"},
		{"--edge--case--", "edge-case"},
		{"", ""},
	}
	for _, c := range cases {
		got := strings.Join(Clean([]string{c.in}), "|")
		if got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := strings.Join(Clean([]string{"gdpr", "GDPR", " gdpr "}), "|"); got != "gdpr" {
		t.Errorf("duplicates survived: %q", got)
	}
	if got := Clean(nil); got == nil || len(got) != 0 {
		t.Errorf("Clean(nil) = %v, want an empty non-nil slice", got)
	}
	long := strings.Repeat("x", MaxLen+10)
	if got := Clean([]string{long})[0]; len(got) != MaxLen {
		t.Errorf("tag not capped: %d chars", len(got))
	}
	many := make([]string, MaxCount+5)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + strings.Repeat("y", i)
	}
	if got := len(Clean(many)); got != MaxCount {
		t.Errorf("count not capped: %d tags", got)
	}
}

func TestSplitAndUnion(t *testing.T) {
	if got := strings.Join(Split(" GDPR , iso 27001 "), "|"); got != "gdpr|iso-27001" {
		t.Errorf("Split = %q", got)
	}
	if got := Split("  "); got != nil {
		t.Errorf("Split of blank = %v, want nil", got)
	}
	if got := strings.Join(Union([]string{"a", "b"}, nil, []string{"B", "c"}), "|"); got != "a|b|c" {
		t.Errorf("Union = %q", got)
	}
}
