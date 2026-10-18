package flomorphic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The profiles listing is read per bucket: the node drawer's own filter, which
// is what makes "this install has nothing to bind this node to" answerable.
func TestListSettingsProfiles(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		// One usable profile, and a row with no id — FloMorphic's bucket
		// placeholder, which is not something a node can be bound to.
		_, _ = w.Write([]byte(`{"data":{"list":[
			{"id":"nset_1","nodeUniqId":"ext:plug-1","nodeType":"plugin","title":"acme GitHub"},
			{"id":"","nodeUniqId":"ext:plug-1","nodeType":"plugin","title":""}
		],"total":2},"error":null}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret")
	if c == nil {
		t.Fatal("client not built")
	}
	list, err := c.ListSettingsProfiles(context.Background(), "ext:plug-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/settings?node=ext%3Aplug-1&per_page=200" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth == "" || gotAccept != "application/json" {
		t.Errorf("auth = %q, accept = %q", gotAuth, gotAccept)
	}
	if len(list) != 1 || list[0].ID != "nset_1" || list[0].Title != "acme GitHub" {
		t.Fatalf("profiles = %+v", list)
	}

	if _, err := c.ListSettingsProfiles(context.Background(), "  "); err == nil {
		t.Error("a bucket is required")
	}
}

func TestListSettingsProfilesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "secret").ListSettingsProfiles(context.Background(), "llm"); err == nil {
		t.Fatal("a 500 must not read as an empty bucket")
	}
}

// Buckets are how FloMorphic keys a profile: per plugin identity for an
// imported plugin's nodes, per kind for a builtin.
func TestSettingsBucket(t *testing.T) {
	if got := SettingsBucket("plugin", "plug-1"); got != "ext:plug-1" {
		t.Errorf("plugin bucket = %q", got)
	}
	if got := SettingsBucket("llm", ""); got != "llm" {
		t.Errorf("builtin bucket = %q", got)
	}
	if got := SettingsBucket("cast", "  "); got != "cast" {
		t.Errorf("blank plugin id must fall back to the kind: %q", got)
	}
}
