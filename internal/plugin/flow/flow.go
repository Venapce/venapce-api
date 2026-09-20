// Package flow holds the small helpers the venapce plugin's action and meta
// handlers share: resolving {{$.path}} tokens against the running flow's scope,
// decoding the flat body a form's meta button sends, and re-rendering a form
// with a multi-select the SDK's formkit does not model. It imports sdkv1 and
// formkit but nothing else in internal/plugin, so both the db and osquery
// modules can depend on it without an import cycle.
package flow

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// Resolver rewrites {{$...}} tokens in free-text inputs against the flow scope,
// so an action's fields can reference data produced by upstream nodes. It caches
// per call, so each distinct path is fetched from the runtime only once however
// many fields reference it.
type Resolver struct {
	job   *sdkv1.Job
	cache map[string]string
}

func NewResolver(job *sdkv1.Job) *Resolver {
	return &Resolver{job: job, cache: make(map[string]string)}
}

// Resolve substitutes every {{$...}} token in text. A token the scope cannot
// supply is left verbatim so nothing is silently dropped.
func (r *Resolver) Resolve(text string) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	return varRe.ReplaceAllStringFunc(text, func(tok string) string {
		path := strings.TrimSpace(varRe.FindStringSubmatch(tok)[1])
		v, ok := r.cache[path]
		if !ok {
			v = r.fetch(path)
			r.cache[path] = v
		}
		return v
	})
}

// fetch reads a JSON path from the flow context. The reply is JSON: a JSON string
// is unwrapped to its value; anything else is returned raw so it can be inlined.
// The path is forwarded as-is — runtime aliases such as $this are the runtime's
// to interpret, not ours. A reply we cannot use is logged, since the token is
// then passed through verbatim and that is what the action will act on.
func (r *Resolver) fetch(jsonPath string) string {
	reply := r.job.CmdGetScope(jsonPath)
	raw, ok := reply.([]byte)
	if !ok || len(raw) == 0 {
		log.Printf("flow: scope %s unresolved for job %s (runtime replied %T: %v); token left verbatim", jsonPath, r.job.JobId, reply, reply)
		return fmt.Sprintf("{{%s}}", jsonPath) // leave the token in place
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// ResolveStruct walks a decoded action input and rewrites {{$...}} tokens in
// every settable string and []string field. Plain fields (no token) never hit
// the runtime, so it is safe to run over every action's input uniformly. `in`
// must be a non-nil pointer to a struct.
func ResolveStruct(job *sdkv1.Job, in any) {
	v := reflect.ValueOf(in)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return
	}
	r := NewResolver(job)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch {
		case f.Kind() == reflect.String:
			f.SetString(r.Resolve(f.String()))
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			for j := 0; j < f.Len(); j++ {
				f.Index(j).SetString(r.Resolve(f.Index(j).String()))
			}
		}
	}
}

// DecodeMeta reads a meta RPC's arguments. Meta calls come from the form renderer
// rather than the job pipeline, so the payload may or may not be wrapped in the
// {_registry, body} envelope — try both, and treat anything unreadable as "no
// arguments" rather than an error.
func DecodeMeta[T any](data []byte) T {
	var out T
	if len(strings.TrimSpace(string(data))) == 0 {
		return out
	}
	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Body) > 0 {
		if json.Unmarshal(envelope.Body, &out) == nil {
			return out
		}
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// MetaString reads a string field from a meta call's flat body, or "" if it is
// absent or not a string.
func MetaString(call map[string]any, key string) string {
	if v, ok := call[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// MetaStrings reads a string-array field from a meta call's flat body — the
// current value of a multi-select — or nil if it is absent or not an array.
// Non-string entries are skipped, blanks trimmed away.
func MetaStrings(call map[string]any, key string) []string {
	items, ok := call[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// ChooseMany is the multi-select counterpart of formkit.Choose: it answers a
// picker lookup with the form re-rendered so that `target` — which must be an
// array property — becomes a multi-select of the given options.
//
// formkit.Choices only knows how to turn a scalar into a drop-down (it sets
// `oneOf` on the property itself). JSON Forms renders an array as a multi-select
// when the property has `uniqueItems: true` and its `items` carry the `oneOf`
// candidates, so this rewrites the property that way instead. Everything else
// about the envelope — echoing the form's current data, the heading, the text
// fallback when the form cannot be rebuilt — matches formkit.Choose.
func ChooseMany(form sdkv1.FormBuilder, target string, options []formkit.Option, data map[string]any, heading formkit.Notification) any {
	envelope, err := pickMany(form, target, options, data, heading)
	if err != nil {
		return formkit.Notification{
			Severity: heading.Severity,
			Field:    heading.Field,
			Message:  strings.TrimSpace(strings.TrimRight(heading.Message, "\n") + "\n" + formkit.Lines(options)),
		}.Patch(nil)
	}
	return envelope
}

func pickMany(form sdkv1.FormBuilder, target string, options []formkit.Option, data map[string]any, heading formkit.Notification) (map[string]any, error) {
	if form.Jsonschema == "" {
		return nil, fmt.Errorf("flow: cannot rebuild a form that has no schema")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(form.Jsonschema), &schema); err != nil {
		return nil, fmt.Errorf("flow: form schema does not parse: %w", err)
	}
	properties, _ := schema["properties"].(map[string]any)
	property, _ := properties[target].(map[string]any)
	if property == nil || property["type"] != "array" {
		return nil, fmt.Errorf("flow: the form has no array property %q to turn into a multi-select", target)
	}
	items, _ := property["items"].(map[string]any)
	if items == nil {
		items = map[string]any{"type": "string"}
	}
	choices := make([]any, 0, len(options))
	for _, o := range options {
		label := o.Label
		if label == "" {
			label = o.Value
		}
		choices = append(choices, map[string]any{"const": o.Value, "title": label})
	}
	items["oneOf"] = choices
	delete(items, "enum")
	property["items"] = items
	property["uniqueItems"] = true

	envelope := map[string]any{
		"schema":   schema,
		"uischema": form.Jsonui,
		"data":     data,
	}
	if heading.Message != "" {
		envelope[formkit.NotifKey] = heading
	}
	return envelope, nil
}
