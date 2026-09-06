// Package flow holds the small helpers the venapce plugin's action and meta
// handlers share: resolving {{$.path}} tokens against the running flow's scope,
// and decoding the flat body a form's meta button sends. It imports sdkv1 but
// nothing else in internal/plugin, so both the db and osquery modules can depend
// on it without an import cycle.
package flow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

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
func (r *Resolver) fetch(jsonPath string) string {
	raw, ok := r.job.CmdGetScope(jsonPath).([]byte)
	if !ok || len(raw) == 0 {
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
