package httpapi

import (
	"errors"
	"testing"

	"github.com/Venapce/venapce-api/internal/config"
)

// The web app address is handed to a browser verbatim, so the two shapes an
// operator reaches for by reflex — a bare host:port, and FloMorphic's container
// name as it appears in the API field right above — have to be rejected at the
// point of entry. Neither resolves from a browser outside the compose network,
// and the only other moment anyone finds out is a dead tab opened from a failed
// run.
func TestNormalizePanelURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{name: "host and port", in: "http://localhost:8088", want: "http://localhost:8088"},
		{name: "the stock local default", in: config.DefaultFlomorphicPanelURL, want: "http://localhost:8088"},
		{name: "trailing slash dropped", in: "http://localhost:8088/", want: "http://localhost:8088"},
		{name: "several trailing slashes dropped", in: "http://localhost:8088///", want: "http://localhost:8088"},
		{name: "surrounding space trimmed", in: "  http://10.0.0.12:8088  ", want: "http://10.0.0.12:8088"},
		{name: "https behind a proxy", in: "https://flomorphic.example.com", want: "https://flomorphic.example.com"},
		{name: "uppercase scheme accepted", in: "HTTP://localhost:8088", want: "HTTP://localhost:8088"},

		{name: "empty is rejected", in: "", err: errPanelURLRequired},
		{name: "blank is rejected", in: "   ", err: errPanelURLRequired},
		{name: "bare host:port is rejected", in: "localhost:8088", err: errPanelURLScheme},
		{name: "container name is rejected", in: "flomorphic", err: errPanelURLScheme},
		{name: "scheme-relative is rejected", in: "//localhost:8088", err: errPanelURLScheme},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizePanelURL(c.in)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("normalizePanelURL(%q) error = %v, want %v", c.in, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePanelURL(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("normalizePanelURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The default has to stay the port the standard FloMorphic compose publishes
// (its canvas on 8088), since that is the whole reason the links work on a fresh
// single-host install. The front carries the same literal in DEFAULT_PANEL_URL,
// so a change here is a change in two places.
func TestDefaultFlomorphicPanelURLIsThePublishedComposePort(t *testing.T) {
	if config.DefaultFlomorphicPanelURL != "http://localhost:8088" {
		t.Fatalf("default panel URL = %q, want http://localhost:8088 (the standard compose's published port; keep DEFAULT_PANEL_URL in venapce-wapp in step)", config.DefaultFlomorphicPanelURL)
	}
}
