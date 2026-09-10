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

// Post sends a write to Superset (e.g. POST /dataset/) and returns the response
// body verbatim (typically {id, result}). Auth/refresh/CSRF are handled by call.
func (c *Client) Post(ctx context.Context, path string, body json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- venapce control blueprint (mounted at /venapce, outside /api/v1) ----

// ExamplesStatus fetches the on-demand example-load status from the Venapce
// control blueprint inside Superset. Returns Superset's HTTP status alongside
// the JSON body so the handler can pass both straight back to the front.
func (c *Client) ExamplesStatus(ctx context.Context) (int, json.RawMessage, error) {
	return c.control(ctx, http.MethodGet, "/venapce/examples/status", nil)
}

// LoadExamples asks Superset to load its example datasets/charts/dashboards in
// the background (idempotent; 202 started, 200 already loaded, 409 in progress).
func (c *Client) LoadExamples(ctx context.Context) (int, json.RawMessage, error) {
	return c.control(ctx, http.MethodPost, "/venapce/examples/load", nil)
}

// control runs an authenticated request against a path OUTSIDE /api/v1 (the
// Venapce blueprint), retrying once through a token refresh on 401.
func (c *Client) control(ctx context.Context, method, path string, body any) (int, json.RawMessage, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return 0, nil, err
	}
	status, data, err := c.doControl(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if isAuthFailure(status, data) {
		if err := c.refresh(ctx); err != nil {
			return 0, nil, err
		}
		if status, data, err = c.doControl(ctx, method, path, body); err != nil {
			return 0, nil, err
		}
	}
	return status, json.RawMessage(data), nil
}

func (c *Client) doControl(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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
	if isAuthFailure(status, data) {
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

// isAuthFailure reports whether a Superset response means "this token is no
// longer good" rather than "this request was bad", i.e. whether it is worth
// re-authenticating and trying once more.
//
// Superset answers an *expired* token with 401, but a token it cannot verify at
// all — malformed, or signed with a key it no longer holds, which is what a
// Superset restart with a fresh SECRET_KEY leaves behind — with 422. Without
// this, such a token is never replaced: ensureAuth only logs in when the token
// is empty, so the client would keep replaying a dead token until the process
// restarts.
//
// 422 is also Superset's ordinary validation status ("Dataset already exists"),
// which must NOT trigger a re-login, so the two are told apart by the body:
// flask-jwt-extended reports {"msg": ...} while the REST API reports
// {"message": ...} / {"errors": [...]}.
func isAuthFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusUnprocessableEntity {
		return false
	}
	var probe struct {
		Msg     json.RawMessage `json:"msg"`
		Message json.RawMessage `json:"message"`
		Errors  json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return len(probe.Msg) > 0 && len(probe.Message) == 0 && len(probe.Errors) == 0
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
