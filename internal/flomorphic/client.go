// Package flomorphic is a small server-side client for the FloMorphic API.
// Venapce runs as a FloMorphic plugin; the backend drives the whole plugin
// lifecycle against the FloMorphic API instead of an operator doing it by hand:
// it registers the extension row (which mints the plugin's inflowv1 identity),
// mints a runtime credential for that identity, and — once the in-process plugin
// is connected — syncs the plugin's @actions into palette nodes. It authenticates
// with an HS256 admin bearer token signed with the shared FloMorphic secret, so
// the secret never reaches the browser.
package flomorphic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Client struct {
	baseURL   string
	jwtSecret string
	http      *http.Client
}

// EnvVar is one extra environment entry shipped in the minted plugin env beyond
// the three the inflowv1 SDK requires (PLUGIN_ID / INFRA_URL / INFRA_CRED).
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NewClient builds a client, or returns nil when the FloMorphic access is not
// configured (empty base URL or secret) so callers can treat "not configured"
// as a nil client.
func NewClient(baseURL, jwtSecret string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(jwtSecret) == "" {
		return nil
	}
	return &Client{
		baseURL:   baseURL,
		jwtSecret: jwtSecret,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

// BaseURL is the configured FloMorphic API base.
func (c *Client) BaseURL() string { return c.baseURL }

// token mints the HS256 admin bearer FloMorphic's API auth verifies against the
// shared secret. A bare {admin:true} claim set is all FloMorphic requires.
func (c *Client) token() (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"admin": true})
	return tok.SignedString([]byte(c.jwtSecret))
}

// do performs one authenticated request and unwraps FloMorphic's {data,error}
// envelope, returning the raw `data`. body is JSON-encoded when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}

	tok, err := c.token()
	if err != nil {
		return nil, fmt.Errorf("sign FloMorphic token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach FloMorphic at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: truncate(data)}
	}

	var env struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("invalid FloMorphic response: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("FloMorphic error: %s", truncate(env.Error))
	}
	return env.Data, nil
}

// HTTPError is a non-2xx response from FloMorphic; NotFound lets callers detect a
// deleted/absent extension row (e.g. to re-create it) rather than string-matching.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("FloMorphic: %d %s", e.Status, e.Body) }
func (e *HTTPError) NotFound() bool { return e.Status == http.StatusNotFound }

// Extension is the subset of a FloMorphic extension row venapce tracks: the row
// id (to sync/delete it) and the inflowv1 PluginID FloMorphic assigned it (to
// mint a credential for and connect the plugin as).
type Extension struct {
	ID       string `json:"id"`
	PluginID string `json:"pluginId"`
}

// CreateExtension registers venapce as a plugin-backed palette extension and
// returns the created row. FloMorphic assigns the PluginID (name-<uuid>) and
// ignores any we send, so the caller does not choose it.
func (c *Client) CreateExtension(ctx context.Context, name, description string) (Extension, error) {
	body := map[string]any{
		"kind":        "extension",
		"type":        "plugin",
		"name":        name,
		"description": description,
		"icon":        map[string]any{"class": "flomorphic", "name": "plug", "meta": map[string]any{}},
	}
	data, err := c.do(ctx, http.MethodPost, "/extension", body)
	if err != nil {
		return Extension{}, err
	}
	var ext Extension
	if err := json.Unmarshal(data, &ext); err != nil {
		return Extension{}, fmt.Errorf("invalid extension response: %w", err)
	}
	if ext.ID == "" || ext.PluginID == "" {
		return Extension{}, fmt.Errorf("FloMorphic returned an extension with no id/pluginId")
	}
	return ext, nil
}

// DeleteExtension removes the extension row (and its synced action rows). A
// missing row is treated as already-deleted (no error), so refresh is safe to
// call whether or not a row still exists.
func (c *Client) DeleteExtension(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	_, err := c.do(ctx, http.MethodDelete, "/extension/id/"+id, nil)
	var he *HTTPError
	if err != nil && asHTTP(err, &he) && he.NotFound() {
		return nil
	}
	return err
}

// SyncResult reports what a sync did to the plugin's palette rows.
type SyncResult struct {
	Added    int    `json:"added"`
	Removed  int    `json:"removed"`
	PluginID string `json:"pluginId"`
}

// SyncExtension rebuilds the plugin's palette nodes from its live @actions. The
// plugin must be connected to infra (answering over NATS) or FloMorphic returns a
// 502 — call this only after the in-process plugin has started.
func (c *Client) SyncExtension(ctx context.Context, id string) (SyncResult, error) {
	data, err := c.do(ctx, http.MethodPost, "/extension/id/"+id+"/sync", struct{}{})
	if err != nil {
		return SyncResult{}, err
	}
	var res SyncResult
	if err := json.Unmarshal(data, &res); err != nil {
		return SyncResult{}, fmt.Errorf("invalid sync response: %w", err)
	}
	return res, nil
}

// ProbeExtension asks FloMorphic to reach the plugin over inflowv1 and return its
// @actions — the same liveness probe the FloMorphic portal uses. A successful
// answer means FloMorphic can see the plugin (it is connected and serving); the
// count is how many palette actions it currently exposes. FloMorphic is the
// reference for "is the plugin connected", so this is checked through it rather
// than by inspecting venapce's own NATS socket. @actions is used (not @intro)
// because every SDK version answers it.
func (c *Client) ProbeExtension(ctx context.Context, id string) (actions int, err error) {
	data, err := c.do(ctx, http.MethodGet, "/extension/id/"+id+"/actions", nil)
	if err != nil {
		return 0, err
	}
	var acts []json.RawMessage
	if err := json.Unmarshal(data, &acts); err != nil {
		return 0, fmt.Errorf("invalid @actions response: %w", err)
	}
	return len(acts), nil
}

// credResponse is the {env,cred} payload POST /extension/plugin/cred returns.
type credResponse struct {
	Env  string `json:"env"`
	Cred string `json:"cred"`
}

// MintPluginEnv mints a strict-access runtime credential for pluginID and returns
// the rendered plugin dotenv (env) and the raw credential (cred). Extra vars —
// e.g. an INFRA_URL override — are shipped in env alongside the credential.
func (c *Client) MintPluginEnv(ctx context.Context, pluginID string, extra []EnvVar) (env, cred string, err error) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return "", "", fmt.Errorf("pluginId is required")
	}
	body := map[string]any{
		"pluginId": pluginID,
		"name":     pluginID,
		"access":   "strict",
		"env":      extra,
	}
	data, err := c.do(ctx, http.MethodPost, "/extension/plugin/cred", body)
	if err != nil {
		return "", "", err
	}
	var out credResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", fmt.Errorf("invalid FloMorphic response: %w", err)
	}
	if strings.TrimSpace(out.Env) == "" {
		return "", "", fmt.Errorf("FloMorphic returned an empty plugin env")
	}
	return out.Env, out.Cred, nil
}

// asHTTP reports whether err is (or wraps) an *HTTPError, binding it to target.
func asHTTP(err error, target **HTTPError) bool {
	for err != nil {
		if he, ok := err.(*HTTPError); ok {
			*target = he
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
