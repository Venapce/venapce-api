package superset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"401 missing header", http.StatusUnauthorized, `{"msg":"Missing Authorization Header"}`, true},
		{"401 expired", http.StatusUnauthorized, `{"msg":"Token has expired"}`, true},
		// Superset 6 answers an unverifiable token with 422, not 401.
		{"422 bad signature", http.StatusUnprocessableEntity, `{"msg":"Signature verification failed"}`, true},
		{"422 malformed", http.StatusUnprocessableEntity, `{"msg":"Not enough segments"}`, true},
		// ...but 422 is also the ordinary validation status, which must not re-login.
		{"422 duplicate dataset", http.StatusUnprocessableEntity, `{"message":{"table":["Dataset already exists"]}}`, false},
		{"422 missing field", http.StatusUnprocessableEntity, `{"message":{"table_name":["Missing data for required field."]}}`, false},
		{"422 errors envelope", http.StatusUnprocessableEntity, `{"errors":[{"message":"boom"}]}`, false},
		{"422 non-JSON", http.StatusUnprocessableEntity, `<html>gateway</html>`, false},
		{"200", http.StatusOK, `{"result":[]}`, false},
		{"500", http.StatusInternalServerError, `{"msg":"kaboom"}`, false},
	}
	for _, c := range cases {
		if got := isAuthFailure(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: isAuthFailure(%d, %s)=%v want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// A token Superset can no longer verify (it restarted with a fresh SECRET_KEY)
// comes back as 422. The client must notice, re-authenticate and retry, rather
// than replaying the dead token until the process restarts.
func TestCallReauthenticatesOn422(t *testing.T) {
	var logins, rejected int32
	goodToken := "fresh-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/security/login":
			atomic.AddInt32(&logins, 1)
			json.NewEncoder(w).Encode(map[string]string{"access_token": goodToken, "refresh_token": "rt"})
		case r.URL.Path == "/api/v1/security/csrf_token/":
			json.NewEncoder(w).Encode(map[string]string{"result": "csrf"})
		case r.URL.Path == "/api/v1/security/refresh":
			// The stale key invalidates the refresh token too, as it would in Superset.
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"msg":"Signature verification failed"}`))
		case r.URL.Path == "/api/v1/database/":
			if r.Header.Get("Authorization") != "Bearer "+goodToken {
				atomic.AddInt32(&rejected, 1)
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"msg":"Signature verification failed"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "admin", "admin")
	// Seed the state a restarted Superset leaves behind: a token it cannot verify.
	c.accessToken, c.refreshToken = "stale-token", "stale-refresh"

	if _, err := c.Result(context.Background(), "/database/"); err != nil {
		t.Fatalf("Result() after a stale token: %v", err)
	}
	if rejected != 1 {
		t.Errorf("stale token presented %d times, want 1", rejected)
	}
	if logins != 1 {
		t.Errorf("logins=%d, want 1 (refresh fails, so it must fall back to a full login)", logins)
	}
}

// A 422 that is ordinary validation must surface as an error, without a re-login.
func TestCallDoesNotReauthenticateOnValidation422(t *testing.T) {
	var logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/security/login":
			atomic.AddInt32(&logins, 1)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "t", "refresh_token": "rt"})
		case "/api/v1/security/csrf_token/":
			json.NewEncoder(w).Encode(map[string]string{"result": "csrf"})
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":{"table":["Dataset already exists"]}}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "admin", "admin")
	_, err := c.Post(context.Background(), "/dataset/", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Post() succeeded, want the validation error surfaced")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to carry Superset's validation message", err)
	}
	if logins != 1 {
		t.Errorf("logins=%d, want 1 (the initial auth only — no re-login on a validation 422)", logins)
	}
}
