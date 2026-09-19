package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sources: where a package comes from, and reading it from there. The wapp
// can read a public GitHub folder itself (raw.githubusercontent.com is
// CORS-open) and send the bundle; this is the server-side twin for the URL
// import path and for "Update" (re-reading a package from its source).

// Source records where an installed operation came from, so it can be
// updated from there.
type Source struct {
	Kind   string `json:"kind"` // url | folder | paste
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Folder string `json:"folder,omitempty"`
}

// Resolved is a URL turned into a fetchable folder.
type Resolved struct {
	Source Source
	// Base the package files are read from (ends with /).
	RawBase string
	// Set for a repository root: where its catalog would be.
	CatalogAt string
}

// ResolveURL understands the same shapes as the wapp:
//
//	https://github.com/org/repo                         → repo root (catalog or package)
//	https://github.com/org/repo/tree/<ref>/<path>       → that folder
//	https://github.com/org/repo/blob/<ref>/<path>/operation.json
//	https://raw.githubusercontent.com/org/repo/<ref>/<path>/…
//	https://host/any/folder/  or  …/operation.json      → a static folder
func ResolveURL(input, defaultRef string) (*Resolved, error) {
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("not an http(s) URL")
	}
	if defaultRef == "" {
		defaultRef = "main"
	}
	strip := func(p string) string {
		p = strings.TrimRight(p, "/")
		p = strings.TrimSuffix(p, "/"+ManifestFile)
		return strings.Trim(p, "/")
	}
	seg := func(p string) []string {
		var out []string
		for _, s := range strings.Split(p, "/") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	switch strings.ToLower(u.Host) {
	case "github.com", "www.github.com":
		parts := seg(u.Path)
		if len(parts) < 2 {
			return nil, errors.New("a GitHub URL needs at least org/repo")
		}
		org, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		ref, path := defaultRef, ""
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
			ref = parts[3]
			path = strip(strings.Join(parts[4:], "/"))
		}
		base := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/", org, repo, ref)
		if path != "" {
			base += path + "/"
		}
		r := &Resolved{Source: Source{Kind: "url", URL: "https://github.com/" + org + "/" + repo, Ref: ref, Path: path}, RawBase: base}
		if path == "" {
			r.CatalogAt = base + CatalogFile
		}
		return r, nil
	case "raw.githubusercontent.com":
		parts := seg(u.Path)
		if len(parts) < 3 {
			return nil, errors.New("a raw GitHub URL needs org/repo/ref")
		}
		org, repo, ref := parts[0], parts[1], parts[2]
		path := strip(strings.Join(parts[3:], "/"))
		base := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/", org, repo, ref)
		if path != "" {
			base += path + "/"
		}
		return &Resolved{Source: Source{Kind: "url", URL: "https://github.com/" + org + "/" + repo, Ref: ref, Path: path}, RawBase: base}, nil
	}
	folder := strip(u.Path)
	base := u.Scheme + "://" + u.Host + "/"
	if folder != "" {
		base += folder + "/"
	}
	return &Resolved{Source: Source{Kind: "url", URL: base}, RawBase: base}, nil
}

// Fetcher reads package files over HTTP.
type Fetcher struct {
	HTTP *http.Client
}

func NewFetcher() *Fetcher {
	return &Fetcher{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// get returns the body, or nil for a 404.
func (f *Fetcher) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("%d fetching %s", res.StatusCode, u)
	}
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}

// CatalogEntry is one operation a catalog.json lists.
type CatalogEntry struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Name        string   `json:"name,omitempty"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Scale       []string `json:"scale,omitempty"`
}

// Catalog reads a repository root's catalog, or nil when it has none.
func (f *Fetcher) Catalog(ctx context.Context, r *Resolved) (name string, entries []CatalogEntry, err error) {
	if r.CatalogAt == "" {
		return "", nil, nil
	}
	body, err := f.get(ctx, r.CatalogAt)
	if err != nil || body == nil {
		return "", nil, err
	}
	var cat struct {
		Name       string         `json:"name"`
		Operations []CatalogEntry `json:"operations"`
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		return "", nil, fmt.Errorf("invalid %s: %w", CatalogFile, err)
	}
	return cat.Name, cat.Operations, nil
}

// Bundle fetches a package folder: the manifest, then every file it names.
// Missing files are reported through Bundle.Check, not here, so the caller
// gets one consolidated problem list.
func (f *Fetcher) Bundle(ctx context.Context, rawBase string) (*Bundle, []string, error) {
	body, err := f.get(ctx, rawBase+ManifestFile)
	if err != nil {
		return nil, nil, err
	}
	if body == nil {
		return nil, nil, fmt.Errorf("no %s at %s", ManifestFile, rawBase)
	}
	m, errs := ParseManifest(body)
	if m == nil {
		return nil, nil, errors.New(strings.Join(errs, "; "))
	}
	files := map[string]string{}
	for _, path := range m.Files() {
		b, err := f.get(ctx, rawBase+strings.TrimLeft(path, "/"))
		if err != nil {
			return nil, nil, err
		}
		if b != nil {
			files[path] = string(b)
		}
	}
	return &Bundle{Manifest: m, Files: files}, errs, nil
}
