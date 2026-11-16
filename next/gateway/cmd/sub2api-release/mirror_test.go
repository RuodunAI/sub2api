package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var mirrorBlobs = map[int]string{1: `{"key_id":"k"}`, 2: "core bundle", 3: "old manifest", 4: "gateway manifest", 5: "gateway archive"}

// fakeGitHub serves a release listing whose assets redirect to a CDN origin,
// like api.github.com does; the CDN fails the test if it receives the token.
func fakeGitHub(t *testing.T) (api *httptest.Server) {
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token sent to the asset CDN")
		}
		var id int
		fmt.Sscanf(r.URL.Path, "/blob/%d", &id)
		_, _ = w.Write([]byte(mirrorBlobs[id]))
	}))
	t.Cleanup(cdn.Close)
	asset := func(id int, name string) map[string]any {
		return map[string]any{"id": id, "name": name, "size": len(mirrorBlobs[id])}
	}
	listing := []map[string]any{
		{"tag_name": "core-v0.1.292", "draft": true, "assets": []any{}},
		{"tag_name": "core-v0.1.291", "published_at": "2026-11-20T08:00:00Z", "body": "notes 291", "html_url": "https://github.com/o/r/releases/tag/core-v0.1.291",
			"assets": []any{asset(1, "next-core-manifest.json"), asset(2, strings.Repeat("b", 64)+".tar.gz")}},
		{"tag_name": "gateway-v0.1.137-rc.1", "prerelease": true, "assets": []any{}},
		{"tag_name": "gateway-v0.1.136", "published_at": "2026-11-19T08:00:00Z", "assets": []any{asset(4, "next-gateway-manifest.json"), asset(5, "sub2api-gateway_0.1.136_linux_amd64.tar.gz")}},
		{"tag_name": "core-v0.1.290", "published_at": "2026-11-17T08:00:00Z", "assets": []any{asset(3, "next-core-manifest.json")}},
		{"tag_name": "v0.1.8", "assets": []any{asset(3, "next-core-manifest.json")}},
	}
	api = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var id int
		switch {
		case r.URL.Path == "/repos/o/r/releases" && r.URL.Query().Get("page") == "1":
			_ = json.NewEncoder(w).Encode(listing)
		case r.URL.Path == "/repos/o/r/releases":
			_, _ = w.Write([]byte("[]"))
		case r.Header.Get("Accept") == "application/octet-stream" && func() bool { _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/releases/assets/%d", &id); return err == nil }():
			http.Redirect(w, r, fmt.Sprintf("%s/blob/%d", cdn.URL, id), http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func readIndex(t *testing.T, dir string) releaseIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, releasesIndex))
	if err != nil {
		t.Fatal(err)
	}
	var idx releaseIndex
	if err = json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestMirrorGitHubLatestThenTagsMerges(t *testing.T) {
	api := fakeGitHub(t)
	dest := t.TempDir()
	src := mirrorSource{client: api.Client(), api: api.URL, repo: "o/r", secret: "tok"}
	if n, err := src.mirror(context.Background(), dest, nil, 1); err != nil || n != 2 {
		t.Fatalf("latest: %d %v", n, err)
	}
	if n, err := src.mirror(context.Background(), dest, []string{"core-v0.1.290", "core-v0.1.291"}, 0); err != nil || n != 2 {
		t.Fatalf("tags: %d %v", n, err)
	}
	idx := readIndex(t, dest)
	var got []string
	for _, r := range idx.Releases {
		got = append(got, r.Line+"-"+r.Version)
	}
	if idx.Schema != releasesSchema || strings.Join(got, ",") != "core-0.1.291,core-0.1.290,gateway-0.1.136" {
		t.Fatalf("index %s %v", idx.Schema, got)
	}
	core := idx.Releases[0]
	if core.Notes != "notes 291" || core.Page == "" || core.PublishedAt.IsZero() || len(core.Assets) != 2 {
		t.Fatalf("core entry %+v", core)
	}
	for name, a := range core.Assets {
		if a.Path != "core/0.1.291/"+name {
			t.Fatalf("asset path %q", a.Path)
		}
	}
	b, err := os.ReadFile(filepath.Join(dest, "core", "0.1.291", "next-core-manifest.json"))
	if err != nil || string(b) != mirrorBlobs[1] {
		t.Fatalf("manifest copy %q %v", b, err)
	}
	if _, err := src.mirror(context.Background(), dest, []string{"core-v0.1.292"}, 0); err == nil {
		t.Fatal("mirrored a draft")
	}
	src.secret = ""
	if _, err := src.mirror(context.Background(), t.TempDir(), nil, 1); err == nil {
		t.Fatal("listing without the token succeeded")
	}
}

func TestMirrorURLSourceCopiesAMirror(t *testing.T) {
	api := fakeGitHub(t)
	first := t.TempDir()
	gh := mirrorSource{client: api.Client(), api: api.URL, repo: "o/r", secret: "tok"}
	if _, err := gh.mirror(context.Background(), first, nil, 5); err != nil {
		t.Fatal(err)
	}
	files := http.FileServer(http.Dir(first))
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.StripPrefix("/rel", files).ServeHTTP(w, r)
	}))
	defer origin.Close()
	second := t.TempDir()
	base, err := normalizeBase(origin.URL + "/rel")
	if err != nil {
		t.Fatal(err)
	}
	src := mirrorSource{client: origin.Client(), base: base, secret: "s3cret"}
	if n, err := src.mirror(context.Background(), second, nil, 5); err != nil || n != 3 {
		t.Fatalf("url mirror: %d %v", n, err)
	}
	a, b := readIndex(t, first), readIndex(t, second)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("index differs:\n%s\n%s", ja, jb)
	}
	got, err := os.ReadFile(filepath.Join(second, "gateway", "0.1.136", "sub2api-gateway_0.1.136_linux_amd64.tar.gz"))
	if err != nil || string(got) != mirrorBlobs[5] {
		t.Fatalf("archive copy %q %v", got, err)
	}

	// An index naming a path outside the source base is refused.
	a.Releases[0].Assets["next-core-manifest.json"] = indexAsset{Path: "../escape", Size: 1}
	raw, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(first, releasesIndex), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := src.mirror(context.Background(), t.TempDir(), nil, 5); err == nil {
		t.Fatal("accepted a path outside the base")
	}
}

func TestMirrorFlagsAndPaths(t *testing.T) {
	for _, args := range [][]string{
		{"mirror", "--out", "x", "--latest", "1"},
		{"mirror", "--out", "x", "--github", "o/r", "--url", "https://h/", "--latest", "1"},
		{"mirror", "--out", "x", "--github", "o/r"},
		{"mirror", "--out", "x", "--github", "not a repo", "--latest", "1"},
		{"mirror", "--out", "x", "--url", "http://h/", "--latest", "1"},
		{"mirror", "--out", "x", "--url", "https://user:pw@h/", "--latest", "1"},
		{"mirror", "--out", "x", "--github", "o/r", "--latest", "1", "--token-env", "SUB2API_RELEASE_TEST_UNSET"},
	} {
		if err := run(args, nil); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	for p, ok := range map[string]bool{"core/0.1.291/a.json": true, "../a": false, "/a": false, "a//b": false, "a/./b": false, ".hidden": false, "a%2e": false, "https://h/a": false, `a\b`: false} {
		if validPath(p) != ok {
			t.Errorf("validPath(%q) = %v", p, !ok)
		}
	}
}
