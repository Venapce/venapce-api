// Package osctrl is a small server-side client for the osctrl API. Like the
// superset client, it holds the API credentials in the backend and logs in for a
// JWT, so the token never reaches the browser. It backs the Nodes area (enrolled
// systems + enroll commands).
package osctrl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL  string
	username string
	password string
	env      string // default environment for API login/scoping
	http     *http.Client

	mu    sync.Mutex
	token string
}

func NewClient(baseURL, username, password, env string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		env:      env,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) BaseURL() string     { return c.baseURL }
func (c *Client) Username() string    { return c.username }
func (c *Client) Environment() string { return c.env }

// EnrollValues is the assembled enrollment helper set the front renders. It is
// built from several osctrl /enroll/{target} calls (each returns one string).
type EnrollValues struct {
	Secret   string            `json:"secret"`
	Flags    string            `json:"flags"`
	OneLiner map[string]string `json:"oneLiner"`
}

// ---- auth ----

// login exchanges the stored credentials for a JWT. osctrl's /login accepts the
// username/password and returns { token, csrf_token }.
func (c *Client) login(ctx context.Context) error {
	payload := map[string]any{"username": c.username, "password": c.password}
	var out struct {
		Token string `json:"token"`
	}
	status, data, err := c.do(ctx, http.MethodPost, "/login", payload, "")
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("osctrl login: %d %s", status, truncate(data, 300))
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	if out.Token == "" {
		return fmt.Errorf("osctrl login: empty token")
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return nil
}

func (c *Client) ensureAuth(ctx context.Context) error {
	c.mu.Lock()
	has := c.token != ""
	c.mu.Unlock()
	if has {
		return nil
	}
	return c.login(ctx)
}

// TestLogin performs a fresh login, used to validate the operator's settings.
func (c *Client) TestLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	if err := c.login(ctx); err != nil {
		return "", err
	}
	return c.username, nil
}

// ---- data ----

// Environments returns the osctrl environments list verbatim (raw JSON), ready
// to hand back to the front.
func (c *Client) Environments(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/environments")
}

// Nodes returns all enrolled nodes for an environment. osctrl answers 404 with a
// "no nodes" body when the environment is empty — normalize that to [].
func (c *Client) Nodes(ctx context.Context, env string) (json.RawMessage, error) {
	raw, err := c.getRaw(ctx, "/nodes/"+env+"/all")
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return json.RawMessage("[]"), nil
		}
		return nil, err
	}
	return raw, nil
}

// Enroll assembles the enrollment helper values for an environment from osctrl's
// per-target endpoints (secret, flags, and the sh/ps1 one-liners).
func (c *Client) Enroll(ctx context.Context, env string) (*EnrollValues, error) {
	secret, err := c.enrollTarget(ctx, env, "secret")
	if err != nil {
		return nil, err
	}
	flags, _ := c.enrollTarget(ctx, env, "flags")
	sh, _ := c.enrollTarget(ctx, env, "enroll.sh")
	ps1, _ := c.enrollTarget(ctx, env, "enroll.ps1")
	return &EnrollValues{
		Secret:   secret,
		Flags:    flags,
		OneLiner: map[string]string{"linux": sh, "darwin": sh, "windows": ps1},
	}, nil
}

// enrollTarget fetches one enrollment value (osctrl returns { "data": "..." }).
func (c *Client) enrollTarget(ctx context.Context, env, target string) (string, error) {
	raw, err := c.getRaw(ctx, "/environments/"+env+"/enroll/"+target)
	if err != nil {
		return "", err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Data, nil
}

// ---- request plumbing ----

// getRaw runs an authenticated GET, retrying once through a re-login on 401.
func (c *Client) getRaw(ctx context.Context, path string) (json.RawMessage, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	status, data, err := c.do(ctx, http.MethodGet, path, nil, tok)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		tok = c.token
		c.mu.Unlock()
		if status, data, err = c.do(ctx, http.MethodGet, path, nil, tok); err != nil {
			return nil, err
		}
	}
	if status >= 400 {
		return nil, fmt.Errorf("osctrl GET %s: %d %s", path, status, truncate(data, 300))
	}
	return data, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, token string) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
