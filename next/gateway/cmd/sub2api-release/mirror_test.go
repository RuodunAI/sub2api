package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/updatesource"
)

var mirrorBlobs = map[int]string{1: `{"core_version":"0.1.291"}`, 2: "core bundle", 3: "old manifest", 4: "gateway manifest", 5: "gateway archive"}

// routed sends every request, whatever its host, to srv; the handler sees the
// requested host in r.Host.
func routed(srv *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}}
}

// fakeGitHub serves api.github.com, whose assets redirect to GitHub's asset
// CDN like the real API; the CDN fails the test if it receives the token.
func fakeGitHub(t *testing.T) *http.Client {
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int
		if r.Host == "release-assets.githubusercontent.com" {
			if r.Header.Get("Authorization") != "" {
				t.Error("token sent to the asset CDN")
			}
			fmt.Sscanf(r.URL.Path, "/blob/%d", &id)
			_, _ = w.Write([]byte(mirrorBlobs[id]))
			return
		}
		if r.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode(listing)
		case r.Header.Get("Accept") == "application/octet-stream" && func() bool { _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/releases/assets/%d", &id); return err == nil }():
			http.Redirect(w, r, fmt.Sprintf("https://release-assets.githubusercontent.com/blob/%d", id), http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return routed(srv)
}

func source(t *testing.T, cfg updatesource.Config, client *http.Client) updatesource.Source {
	t.Helper()
	cfg.Enabled = true
	src, err := updatesource.New(cfg, updatesource.Options{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func readIndex(t *testing.T, dir string) updatesource.Index {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, releasesIndex))
	if err != nil {
		t.Fatal(err)
	}
	var idx updatesource.Index
	if err = json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestMirrorGitHubLatestThenTagsMerges(t *testing.T) {
	client := fakeGitHub(t)
	gh := updatesource.Config{Kind: updatesource.GitHub, Repository: "o/r", Secret: "tok"}
	src := source(t, gh, client)
	dest := t.TempDir()
	if n, err := mirrorReleases(context.Background(), src, dest, nil, 1); err != nil || n != 2 {
		t.Fatalf("latest: %d %v", n, err)
	}
	if n, err := mirrorReleases(context.Background(), src, dest, []string{"core-v0.1.290", "core-v0.1.291"}, 0); err != nil || n != 2 {
		t.Fatalf("tags: %d %v", n, err)
	}
	idx := readIndex(t, dest)
	var got []string
	for _, r := range idx.Releases {
		got = append(got, r.Line+"-"+r.Version)
	}
	if idx.Schema != updatesource.IndexSchema || strings.Join(got, ",") != "core-0.1.291,core-0.1.290,gateway-0.1.136" {
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
	// Whatever serves the directory can read the assets as well as the index.
	for _, p := range []string{filepath.Join("core", "0.1.291", "next-core-manifest.json"), releasesIndex} {
		if st, err := os.Stat(filepath.Join(dest, p)); err != nil || st.Mode().Perm() != 0644 {
			t.Fatalf("%s: %v %v", p, st.Mode(), err)
		}
	}
	if _, err := mirrorReleases(context.Background(), src, dest, []string{"core-v0.1.292"}, 0); err == nil {
		t.Fatal("mirrored a draft")
	}
	gh.Secret = ""
	if _, err := mirrorReleases(context.Background(), source(t, gh, client), t.TempDir(), nil, 1); err == nil {
		t.Fatal("listing without the token succeeded")
	}
}

func TestMirrorURLSourceCopiesAMirror(t *testing.T) {
	first := t.TempDir()
	gh := source(t, updatesource.Config{Kind: updatesource.GitHub, Repository: "o/r", Secret: "tok"}, fakeGitHub(t))
	if _, err := mirrorReleases(context.Background(), gh, first, nil, 5); err != nil {
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
	base, err := updatesource.NormalizeURL("https://mirror.example.com/rel")
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, updatesource.Config{Kind: updatesource.URL, URL: base, Secret: "s3cret"}, routed(origin))
	second := t.TempDir()
	if n, err := mirrorReleases(context.Background(), src, second, nil, 5); err != nil || n != 3 {
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
	a.Releases[0].Assets["next-core-manifest.json"] = updatesource.IndexAsset{Path: "../escape", Size: 1}
	raw, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(first, releasesIndex), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := mirrorReleases(context.Background(), src, t.TempDir(), nil, 5); err == nil {
		t.Fatal("accepted a path outside the base")
	}
}

func TestMirrorFlags(t *testing.T) {
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
}
