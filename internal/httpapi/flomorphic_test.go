package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePluginEnv(t *testing.T) {
	in := `
# a comment
export PLUGIN_ID="pid-123"
INFRA_CRED='cred-xyz'
INFRA_URL=nats://infra:4222
EMPTY=
`
	got := parsePluginEnv(in)
	if got["PLUGIN_ID"] != "pid-123" {
		t.Errorf("PLUGIN_ID = %q", got["PLUGIN_ID"])
	}
	if got["INFRA_CRED"] != "cred-xyz" {
		t.Errorf("INFRA_CRED = %q", got["INFRA_CRED"])
	}
	if got["INFRA_URL"] != "nats://infra:4222" {
		t.Errorf("INFRA_URL = %q", got["INFRA_URL"])
	}
}

func TestDeriveInfraBase(t *testing.T) {
	cases := map[string]string{
		"nats://infra:4222":            "http://infra:8022",
		"tls://user:pass@host.x:4222":  "http://host.x:8022",
		"infra:4222":                   "http://infra:8022",
		"infra":                        "http://infra:8022",
		"nats://a:4222,nats://b:4222":  "http://a:8022",
	}
	for in, want := range cases {
		got, err := deriveInfraBase(in)
		if err != nil {
			t.Errorf("deriveInfraBase(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("deriveInfraBase(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := deriveInfraBase(""); err == nil {
		t.Error("deriveInfraBase(\"\") should error")
	}
}

func TestFetchOsspaceRedirectJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"redirect":"https://accounts.google.com/o/oauth2/x"},"error":null}`))
	}))
	defer srv.Close()

	res, err := fetchOsspace(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != "https://accounts.google.com/o/oauth2/x" {
		t.Errorf("redirect = %q", res.Redirect)
	}
}

func TestFetchOsspaceSpace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"environment":"env-1","hostname":"osctrl.inflowenger.com","username":"u","password":"p","email":"e@x.com"},"error":null}`))
	}))
	defer srv.Close()

	res, err := fetchOsspace(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != "" {
		t.Errorf("unexpected redirect %q", res.Redirect)
	}
	if res.Environment != "env-1" || res.Hostname != "osctrl.inflowenger.com" || res.Username != "u" || res.Password != "p" {
		t.Errorf("space = %+v", res)
	}
}

func TestFetchOsspace302Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://accounts.google.com/legacy", http.StatusFound)
	}))
	defer srv.Close()

	res, err := fetchOsspace(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Redirect, "accounts.google.com") {
		t.Errorf("redirect = %q", res.Redirect)
	}
}

func TestFetchOsspaceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"data":null,"error":{"message":"no active license"}}`))
	}))
	defer srv.Close()

	if _, err := fetchOsspace(context.Background(), srv.URL); err == nil {
		t.Error("expected an error for a 502 response")
	}
}
