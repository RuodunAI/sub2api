package updatesource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testSecret = "tok-secret-123"

// routed sends every request, whatever its host, to srv: the handler sees
// the requested host in r.Host.
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

// requests records what each host received.
type requests struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (r *requests) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Clone(context.Background()))
}
func (r *requests) hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.seen {
		out = append(out, q.Host)
	}
	return out
}

func githubServer(t *testing.T, redirect string, asset []byte) (*httptest.Server, *requests) {
	t.Helper()
	log := &requests{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		switch {
		case r.Host == "api.github.com" && r.URL.Path == "/repos/owner/repo/releases":
			if r.URL.Query().Get("per_page") != "50" {
				t.Errorf("listing query %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"tag_name": "core-v0.1.9", "published_at": "2026-11-01T00:00:00Z", "body": "old", "assets": []map[string]any{{"id": 7, "name": "next-core-manifest.json", "size": 3}}},
				{"tag_name": "core-v0.1.10", "published_at": "2026-11-02T00:00:00Z", "body": "notes", "assets": []map[string]any{{"id": 8, "name": "next-core-manifest.json", "size": len(asset)}, {"id": 9, "name": "../evil", "size": 1}}},
				{"tag_name": "core-v0.2.0", "draft": true},
				{"tag_name": "core-v0.3.0-rc.1", "prerelease": false},
				{"tag_name": "gateway-v0.1.136", "prerelease": true},
				{"tag_name": "gateway-v0.1.135"},
				{"tag_name": "v0.9.0"},
				{"tag_name": "core-v01.2.3"},
			})
		case r.Host == "api.github.com" && r.URL.Path == "/repos/owner/repo/releases/assets/8":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset Accept %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Location", redirect)
			w.WriteHeader(http.StatusFound)
		case r.Host == "release-assets.githubusercontent.com" || r.Host == "objects.githubusercontent.com":
			_, _ = w.Write(asset)
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func TestGitHubReleasesListStableLinesNewestFirst(t *testing.T) {
	srv, log := githubServer(t, "", nil)
	src, err := New(Config{Kind: GitHub, Repository: "owner/repo", Secret: testSecret}, Options{Client: routed(srv)})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := src.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, l := range ls {
		tags = append(tags, l.Tag)
	}
	if got := strings.Join(tags, ","); got != "core-v0.1.10,core-v0.1.9,gateway-v0.1.135" {
		t.Fatalf("listings %s", got)
	}
	latest, ok := Latest(ls, LineCore)
	if !ok || latest.Version != "0.1.10" || latest.Notes != "notes" || latest.Page != "https://github.com/owner/repo/releases/tag/core-v0.1.10" {
		t.Fatalf("latest %+v", latest)
	}
	if _, bad := latest.Assets["../evil"]; bad || latest.Assets["next-core-manifest.json"].Ref != "8" {
		t.Fatalf("assets %+v", latest.Assets)
	}
	if _, ok = Latest(ls, "plugins"); ok {
		t.Fatal("unknown line has a latest release")
	}
	if auth := log.seen[0].Header.Get("Authorization"); auth != "Bearer "+testSecret {
		t.Fatalf("API request auth %q", auth)
	}
}

// pagedGitHub serves pages of 50 core releases, newest first, and then
// lastPage, which holds the only gateway release; full pages never end when
// lastPage is nil.
func pagedGitHub(t *testing.T, fullPages int, lastPage []map[string]any) (*httptest.Server, *requests) {
	t.Helper()
	log := &requests{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.Host != "api.github.com" || r.URL.Path != "/repos/owner/repo/releases" || r.URL.Query().Get("per_page") != "50" || page < 1 {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		if lastPage != nil && page > fullPages {
			_ = json.NewEncoder(w).Encode(lastPage)
			return
		}
		out := make([]map[string]any, 50)
		for i := range out {
			out[i] = map[string]any{"tag_name": fmt.Sprintf("core-v1.%d.%d", 1000-page, 50-i)}
		}
		// The newest release of the page before slid onto this one.
		if page > 1 {
			out[0]["tag_name"] = fmt.Sprintf("core-v1.%d.1", 1000-page+1)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func TestGitHubReleasesFollowPagesWithinACap(t *testing.T) {
	srv, log := pagedGitHub(t, 2, []map[string]any{{"tag_name": "gateway-v0.1.136"}, {"tag_name": "core-v0.9.0"}})
	src, _ := New(Config{Kind: GitHub, Repository: "owner/repo", Secret: testSecret}, Options{Client: routed(srv)})
	ls, err := src.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := Latest(ls, LineGateway); !ok || l.Version != "0.1.136" {
		t.Fatalf("the gateway line on the third page: %+v", l)
	}
	if len(ls) != 50+49+2 {
		t.Fatalf("%d listings: a release repeated across pages is listed once", len(ls))
	}
	if len(log.seen) != 3 {
		t.Fatalf("%d requests: a short page ends the listing", len(log.seen))
	}
	for _, r := range log.seen {
		if r.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer "+testSecret {
			t.Fatalf("page request to %s with %q", r.Host, r.Header.Get("Authorization"))
		}
	}

	endless, log := pagedGitHub(t, 0, nil)
	src, _ = New(Config{Kind: GitHub, Repository: "owner/repo"}, Options{Client: routed(endless)})
	if ls, err = src.Releases(context.Background()); err != nil || len(log.seen) != githubMaxPages || len(ls) != MaxGitHubReleases-githubMaxPages+1 {
		t.Fatalf("endless pages: %d requests, %d listings, %v", len(log.seen), len(ls), err)
	}
}

func TestGitHubAssetRedirectPolicy(t *testing.T) {
	body := []byte("signed manifest")
	for _, target := range []string{
		"https://release-assets.githubusercontent.com/asset?sig=urlsecret",
		"https://objects.githubusercontent.com/asset",
		"https://evil.test/asset?sig=urlsecret",
		"http://release-assets.githubusercontent.com/asset",
		"https://release-assets.githubusercontent.com:443/asset",
		"https://user@objects.githubusercontent.com/asset",
		"https://release-assets.githubusercontent.com.evil.test/asset?sig=urlsecret",
	} {
		t.Run(target, func(t *testing.T) {
			srv, log := githubServer(t, target, body)
			src, _ := New(Config{Kind: GitHub, Repository: "owner/repo", Secret: testSecret}, Options{Client: routed(srv)})
			rc, err := src.Open(context.Background(), Asset{Name: "next-core-manifest.json", Size: int64(len(body)), Ref: "8"}, 1<<20)
			allowed := !strings.Contains(target, "evil") && !strings.HasPrefix(target, "http:") && !strings.Contains(target, ":443") && !strings.Contains(target, "user@")
			if !allowed {
				if err == nil {
					rc.Close()
					t.Fatal("unsafe redirect followed")
				}
				if len(log.seen) != 1 {
					t.Fatalf("unsafe host contacted: %v", log.hosts())
				}
				if strings.Contains(err.Error(), "urlsecret") || strings.Contains(err.Error(), testSecret) {
					t.Fatalf("error leaks a secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if string(got) != string(body) {
				t.Fatalf("body %q", got)
			}
			cdn := log.seen[1]
			if cdn.Header.Get("Authorization") != "" || cdn.Header.Get("X-GitHub-Api-Version") != "" {
				t.Fatalf("API headers reached the CDN: %v", cdn.Header)
			}
			if log.seen[0].Header.Get("Authorization") != "Bearer "+testSecret {
				t.Fatal("API asset request lacks the token")
			}
		})
	}
}

func TestGitHubOpenBoundsAndReferences(t *testing.T) {
	body := []byte(strings.Repeat("x", 100))
	srv, _ := githubServer(t, "https://objects.githubusercontent.com/asset", body)
	src, _ := New(Config{Kind: GitHub, Repository: "owner/repo"}, Options{Client: routed(srv)})
	ctx := context.Background()
	for _, ref := range []string{"", "0", "-1", "08", "8/../9", "abc"} {
		if _, err := src.Open(ctx, Asset{Name: "a", Ref: ref}, 1000); err == nil {
			t.Fatalf("reference %q accepted", ref)
		}
	}
	if _, err := src.Open(ctx, Asset{Name: "a", Ref: "8", Size: 101}, 100); err == nil {
		t.Fatal("declared oversize asset opened")
	}
	// A body longer than declared fails the read instead of truncating.
	rc, err := src.Open(ctx, Asset{Name: "a", Ref: "8", Size: 50}, 50)
	if err == nil {
		_, err = io.ReadAll(rc)
		rc.Close()
	}
	if err == nil {
		t.Fatal("oversize body read without error")
	}
}

func TestGitHubStatusErrors(t *testing.T) {
	for status, want := range map[int]string{404: "token", 429: "rate limited", 401: "credentials", 500: "HTTP 500"} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		src, _ := New(Config{Kind: GitHub, Repository: "owner/repo", Secret: testSecret}, Options{Client: routed(srv)})
		_, err := src.Releases(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), testSecret) {
			t.Fatalf("HTTP %d: %v", status, err)
		}
	}
}

func indexServer(t *testing.T, index any, handle func(w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, *requests) {
	t.Helper()
	log := &requests{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if handle != nil && handle(w, r) {
			return
		}
		if r.URL.Path == "/rel/releases.json" {
			_ = json.NewEncoder(w).Encode(index)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func indexDoc(assets map[string]any) map[string]any {
	return map[string]any{"schema": IndexSchema, "releases": []map[string]any{
		{"line": "core", "version": "0.1.291", "published_at": "2026-11-20T08:00:00Z", "notes": "n", "page": "https://example.com/r", "assets": assets},
		{"line": "core", "version": "0.1.292-rc.1", "assets": map[string]any{}},
		{"line": "gateway", "version": "0.1.136", "page": "javascript:alert(1)", "assets": map[string]any{}},
		{"line": "plugins", "version": "1.0.0"},
	}}
}

func TestURLIndexListsAndOpensSameOriginAssets(t *testing.T) {
	srv, log := indexServer(t, indexDoc(map[string]any{"next-core-manifest.json": map[string]any{"path": "core/0.1.291/next-core-manifest.json", "size": 2}}), func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/rel/core/0.1.291/next-core-manifest.json":
			http.Redirect(w, r, "/rel/moved.json", http.StatusFound)
		case "/rel/moved.json":
			_, _ = w.Write([]byte("ok"))
		default:
			return false
		}
		return true
	})
	src, err := New(Config{Kind: URL, URL: srv.URL + "/rel/", Secret: testSecret}, Options{Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := src.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[0].Tag != "core-v0.1.291" || ls[1].Tag != "gateway-v0.1.136" || ls[1].Page != "" || ls[0].Page != "https://example.com/r" {
		t.Fatalf("listings %+v", ls)
	}
	rc, err := src.Open(context.Background(), ls[0].Assets["next-core-manifest.json"], 10)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "ok" {
		t.Fatalf("asset %q", got)
	}
	for _, r := range log.seen {
		if r.Header.Get("Authorization") != "Bearer "+testSecret {
			t.Fatalf("same-origin request %s lacks the secret", r.URL.Path)
		}
	}
}

func TestURLIndexRejectsUnsafeAssetPaths(t *testing.T) {
	for _, p := range []string{"../x", "/rel/x", "https://evil.test/x", "a/../../x", "%2e%2e/x", "a//b", "./x", "x?token=1", "x#f", `a\b`, "//evil.test/x", "", "javascript:x"} {
		srv, _ := indexServer(t, indexDoc(map[string]any{"a.tar.gz": map[string]any{"path": p, "size": 1}}), nil)
		src, _ := New(Config{Kind: URL, URL: srv.URL + "/rel/"}, Options{Client: srv.Client()})
		if _, err := src.Releases(context.Background()); err == nil {
			t.Fatalf("path %q accepted", p)
		}
		if rc, err := src.Open(context.Background(), Asset{Name: "a", Ref: p, Size: 1}, 10); err == nil {
			rc.Close()
			t.Fatalf("path %q opened", p)
		}
	}
	for name, doc := range map[string]any{
		"schema":    map[string]any{"schema": "other", "releases": []any{}},
		"duplicate": map[string]any{"schema": IndexSchema, "releases": []map[string]any{{"line": "core", "version": "1.0.0"}, {"line": "core", "version": "1.0.0"}}},
		"size":      indexDoc(map[string]any{"a.tar.gz": map[string]any{"path": "a.tar.gz", "size": 0}}),
		"name":      indexDoc(map[string]any{"../a": map[string]any{"path": "a", "size": 1}}),
	} {
		srv, _ := indexServer(t, doc, nil)
		src, _ := New(Config{Kind: URL, URL: srv.URL + "/rel/"}, Options{Client: srv.Client()})
		if _, err := src.Releases(context.Background()); err == nil {
			t.Fatalf("%s: invalid index accepted", name)
		}
	}
}

func TestURLIndexRefusesCrossOriginRedirect(t *testing.T) {
	other, otherLog := indexServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool { _, _ = w.Write([]byte("x")); return true })
	srv, _ := indexServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, other.URL+"/steal?k=urlsecret", http.StatusFound)
		return true
	})
	src, _ := New(Config{Kind: URL, URL: srv.URL + "/rel/", Secret: testSecret}, Options{Client: srv.Client()})
	_, err := src.Releases(context.Background())
	if err == nil || strings.Contains(err.Error(), "urlsecret") || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("cross-origin redirect: %v", err)
	}
	if rc, err := src.Open(context.Background(), Asset{Name: "a", Ref: "a", Size: 1}, 10); err == nil {
		rc.Close()
		t.Fatal("cross-origin asset redirect followed")
	}
	if len(otherLog.seen) != 0 {
		t.Fatal("other origin contacted")
	}
}

func TestNormalizeRepository(t *testing.T) {
	for _, raw := range []string{"owner/repo", "https://github.com/owner/repo", "https://github.com/owner/repo.git/", " owner/repo "} {
		if got, err := NormalizeRepository(raw); err != nil || got != "owner/repo" {
			t.Fatalf("%s: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "http://github.com/o/r", "https://github.com.evil.test/o/r", "https://user@github.com/o/r", "https://github.com/o/r?token=secret", "https://github.com:443/o/r", "owner/repo/releases", "../repo"} {
		if _, err := NormalizeRepository(raw); err == nil {
			t.Fatalf("invalid repository accepted: %q", raw)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Releases.Example.com/sub2api": "https://releases.example.com/sub2api/",
		"https://r.example.com:8443/a/":        "https://r.example.com:8443/a/",
		"https://r.example.com":                "https://r.example.com/",
	} {
		if got, err := NormalizeURL(raw); err != nil || got != want {
			t.Fatalf("%s: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "http://r.example.com/", "https://u:p@r.example.com/", "https://r.example.com/?token=x", "https://r.example.com/?", "https://r.example.com/#f", "ftp://r.example.com/", "https:///path", "r.example.com"} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Fatalf("invalid url accepted: %q", raw)
		}
	}
}

func TestNewRejectsUnnormalizedConfig(t *testing.T) {
	for _, cfg := range []Config{{Kind: GitHub, Repository: "https://github.com/o/r"}, {Kind: URL, URL: "https://r.example.com"}, {Kind: "s3"}} {
		if _, err := New(cfg, Options{}); err == nil {
			t.Fatalf("%+v accepted", cfg)
		}
	}
}

func TestClientAddressPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "::1", "fd00::1", "::ffff:127.0.0.1",
		// NAT64 and 6to4 of private IPv4, local-use NAT64, Teredo.
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "2002:a00:1::1", "2002:7f00:1::", "64:ff9b:1::808:808", "2001:0:4136:e378::1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatalf("unsafe address accepted: %s", s)
		}
	}
	// IPv6-only nodes reach IPv4-only hosts through well-known NAT64.
	for _, s := range []string{"1.1.1.1", "2606:4700::1111", "64:ff9b::8c52:7904", "2002:808:808::1"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Fatalf("public address rejected: %s", s)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := fmt.Sprintf("localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	if c, err := pinnedDial(false)(context.Background(), "tcp", addr); err == nil {
		c.Close()
		t.Fatal("public-only client dialed a loopback address")
	}
	c, err := pinnedDial(true)(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if Client(false).Transport.(*http.Transport).Proxy != nil {
		t.Fatal("update client uses an environment proxy")
	}
}
