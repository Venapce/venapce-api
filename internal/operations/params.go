package operations

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Params: the operator's values, resolved over the manifest defaults, and
// substituted into flow files. A string that IS a `${param.x}` placeholder is
// replaced by the param's value with its own JSON type (an array param stands
// in for a `tags` list); a placeholder inside a longer string is interpolated
// as text. Unknown params are left as they are, so the author sees them on
// the canvas. Mirrors the wapp's lib/operations.ts substituteParams.

var placeholderRe = regexp.MustCompile(`\$\{param\.([A-Za-z0-9_-]+)\}`)
var wholePlaceholderRe = regexp.MustCompile(`^\$\{param\.([A-Za-z0-9_-]+)\}$`)

// Effective resolves every declared param: the operator's value when set,
// else the default; params with neither are absent.
func Effective(m *Manifest, values map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for name, spec := range m.Params {
		if v, ok := values[name]; ok && len(v) > 0 && string(v) != "null" && string(v) != `""` {
			out[name] = v
			continue
		}
		if len(spec.Default) > 0 && string(spec.Default) != "null" {
			out[name] = spec.Default
		}
	}
	return out
}

// Missing lists required params that have neither a value nor a default.
func Missing(m *Manifest, values map[string]json.RawMessage) []string {
	eff := Effective(m, values)
	var out []string
	for name, spec := range m.Params {
		if spec.Required {
			if _, ok := eff[name]; !ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// Substitute applies the params to a JSON document (a flow file) and returns
// the substituted document, compact-encoded.
func Substitute(doc []byte, params map[string]json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, err
	}
	decoded := map[string]any{}
	for k, raw := range params {
		var x any
		if json.Unmarshal(raw, &x) == nil {
			decoded[k] = x
		}
	}
	return json.Marshal(walk(v, decoded))
}

func walk(v any, params map[string]any) any {
	switch t := v.(type) {
	case string:
		if m := wholePlaceholderRe.FindStringSubmatch(t); m != nil {
			if p, ok := params[m[1]]; ok {
				return p
			}
			return t
		}
		return placeholderRe.ReplaceAllStringFunc(t, func(all string) string {
			name := placeholderRe.FindStringSubmatch(all)[1]
			p, ok := params[name]
			if !ok {
				return all
			}
			if s, isStr := p.(string); isStr {
				return s
			}
			b, _ := json.Marshal(p)
			return string(b)
		})
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = walk(x, params)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = walk(x, params)
		}
		return out
	default:
		return v
	}
}

// SplitSecrets separates the params whose spec is `secret` from the rest, so
// the caller can encrypt them. Names not declared in the manifest are kept as
// plain params.
func SplitSecrets(m *Manifest, values map[string]json.RawMessage) (plain, secret map[string]json.RawMessage) {
	plain, secret = map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for k, v := range values {
		if spec, ok := m.Params[k]; ok && spec.Secret {
			secret[k] = v
		} else {
			plain[k] = v
		}
	}
	return
}

// ParamsDoc decodes a stored params document; a missing / malformed one is empty.
func ParamsDoc(raw json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	return out
}

// TrimEmpty drops params whose value is empty text — the form's way of saying
// "use the default".
func TrimEmpty(values map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range values {
		s := strings.TrimSpace(string(v))
		if s == "" || s == "null" || s == `""` {
			continue
		}
		out[k] = v
	}
	return out
}
