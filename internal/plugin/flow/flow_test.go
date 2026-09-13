package flow

import (
	"encoding/json"
	"testing"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
)

func TestMetaStrings(t *testing.T) {
	call := map[string]any{"tags": []any{" a ", "", 3, "b"}, "env": "x"}
	if got := MetaStrings(call, "tags"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("want [a b], got %v", got)
	}
	if got := MetaStrings(call, "env"); got != nil {
		t.Errorf("non-array must yield nil, got %v", got)
	}
}

func TestChooseManyRewritesArrayItems(t *testing.T) {
	form := formkit.New("f").Add(
		formkit.Text("env", "Env"),
		formkit.List("tags", "Tags").Set("uniqueItems", true),
	).Build()
	options := []formkit.Option{{Value: "linux", Label: "linux (auto)"}, {Value: "web"}}
	data := map[string]any{"env": "corp", "tags": []any{"web"}}

	out, ok := ChooseMany(form, "tags", options, data, formkit.Success("2 tags").About("tags")).(map[string]any)
	if !ok {
		t.Fatalf("want a form envelope, got %T", out)
	}
	for _, key := range []string{"schema", "uischema", "data", formkit.NotifKey} {
		if _, has := out[key]; !has {
			t.Errorf("envelope missing %q", key)
		}
	}
	if out["data"].(map[string]any)["tags"].([]any)[0] != "web" {
		t.Errorf("current selection must be echoed back, got %v", out["data"])
	}
	schema := out["schema"].(map[string]any)
	prop := schema["properties"].(map[string]any)["tags"].(map[string]any)
	if prop["type"] != "array" || prop["uniqueItems"] != true {
		t.Errorf("tags property must stay an array with uniqueItems, got %v", prop)
	}
	items := prop["items"].(map[string]any)
	oneOf, _ := items["oneOf"].([]any)
	if len(oneOf) != 2 {
		t.Fatalf("want 2 oneOf candidates on items, got %v", items)
	}
	first := oneOf[0].(map[string]any)
	if first["const"] != "linux" || first["title"] != "linux (auto)" {
		t.Errorf("unexpected first candidate: %v", first)
	}
	second := oneOf[1].(map[string]any)
	if second["title"] != "web" {
		t.Errorf("label must fall back to the value, got %v", second)
	}
	if _, has := prop["oneOf"]; has {
		t.Errorf("oneOf must live on items, not on the array property itself")
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("envelope must marshal: %v", err)
	}
}

func TestChooseManyFallsBackToTextForScalar(t *testing.T) {
	form := formkit.New("f").Add(formkit.Text("uuid", "Node")).Build()
	out, ok := ChooseMany(form, "uuid", []formkit.Option{{Value: "u1", Label: "web-1"}}, nil, formkit.Info("pick").About("uuid")).(map[string]any)
	if !ok {
		t.Fatalf("want a patch map, got %T", out)
	}
	if _, has := out["schema"]; has {
		t.Errorf("a scalar target must not be re-rendered as a multi-select")
	}
	n, _ := out[formkit.NotifKey].(formkit.Notification)
	if n.Field != "uuid" || n.Message == "" {
		t.Errorf("fallback must carry the candidates as a message about the field, got %#v", n)
	}
}
