// Package superset is a small server-side Superset REST client. It mirrors the
// TS headless client the Vue front used to run in the browser (login -> CSRF ->
// Bearer, transparent refresh on 401), but now lives in the backend so the
// service-account token never reaches the browser (integration study §4).
package superset

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
	http     *http.Client

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	csrfToken    string
}

func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) BaseURL() string  { return c.baseURL }
func (c *Client) Username() string { return c.username }

// ---- auth ----

func (c *Client) login(ctx context.Context) error {
	payload := map[string]any{
		"username": c.username,
		"password": c.password,
		"provider": "db",
		"refresh":  true,
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.rawJSON(ctx, http.MethodPost, "/security/login", payload, false, &out); err != nil {
		return fmt.Errorf("superset login failed: %w", err)
	}
	c.mu.Lock()
	c.accessToken, c.refreshToken = out.AccessToken, out.RefreshToken
	c.mu.Unlock()
	c.fetchCSRF(ctx) // best-effort; server may exempt the API from CSRF
	return nil
}

func (c *Client) fetchCSRF(ctx context.Context) {
	var out struct {
		Result string `json:"result"`
	}
	if err := c.rawJSON(ctx, http.MethodGet, "/security/csrf_token/", nil, true, &out); err == nil {
		c.mu.Lock()
		c.csrfToken = out.Result
		c.mu.Unlock()
	}
}

func (c *Client) refresh(ctx context.Context) error {
	c.mu.Lock()
	rt := c.refreshToken
	c.mu.Unlock()
	if rt == "" {
		return c.login(ctx)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/security/refresh", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rt)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return c.login(ctx) // refresh token expired too -> full re-login
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	c.mu.Lock()
	c.accessToken = out.AccessToken
	c.mu.Unlock()
	return nil
}

func (c *Client) ensureAuth(ctx context.Context) error {
	c.mu.Lock()
	has := c.accessToken != ""
	c.mu.Unlock()
	if has {
		return nil
	}
	return c.login(ctx)
}

// TestLogin performs a fresh login and returns the authenticated username, used
// to validate connection settings the operator entered.
func (c *Client) TestLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.accessToken, c.refreshToken, c.csrfToken = "", "", ""
	c.mu.Unlock()
	if err := c.login(ctx); err != nil {
		return "", err
	}
	var out struct {
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := c.rawJSON(ctx, http.MethodGet, "/me/", nil, true, &out); err != nil {
		return c.username, nil // login worked; /me is a bonus
	}
	if out.Result.Username != "" {
		return out.Result.Username, nil
	}
	return c.username, nil
}

// ---- data / catalog ----

// Result returns the `result` field of a Superset list/detail response as raw
// JSON, ready to hand straight back to the front.
func (c *Client) Result(ctx context.Context, path string) (json.RawMessage, error) {
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// ChartData proxies POST /chart/data, returning Superset's response verbatim.
func (c *Client) ChartData(ctx context.Context, queryContext json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.call(ctx, http.MethodPost, "/chart/data", queryContext, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- request plumbing ----

// call runs an authenticated request, retrying once through a token refresh on 401.
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	status, data, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if err := c.refresh(ctx); err != nil {
			return err
		}
		if status, data, err = c.do(ctx, method, path, body); err != nil {
			return err
		}
	}
	if status >= 400 {
		return fmt.Errorf("superset %s %s: %d %s", method, path, status, truncate(data, 400))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	at, csrf := c.accessToken, c.csrfToken
	c.mu.Unlock()
	if at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRFToken", csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// rawJSON is a bare request used by the auth calls (which must not recurse into
// ensureAuth/refresh). withBearer attaches the current access token.
func (c *Client) rawJSON(ctx context.Context, method, path string, body any, withBearer bool, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if withBearer {
		c.mu.Lock()
		at := c.accessToken
		c.mu.Unlock()
		if at != "" {
			req.Header.Set("Authorization", "Bearer "+at)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%d %s", resp.StatusCode, truncate(data, 400))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		switch b := body.(type) {
		case json.RawMessage:
			r = bytes.NewReader(b)
		case []byte:
			r = bytes.NewReader(b)
		default:
			buf, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			r = bytes.NewReader(buf)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
