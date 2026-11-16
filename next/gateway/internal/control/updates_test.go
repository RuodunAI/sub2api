package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/updatesource"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/secret"
)

// fixtureURL is the url source the fixture serves; every host is routed to
// the fixture's server.
const fixtureURL = "https://releases.example.com/rel/"

var bundleDigest = strings.Repeat("a", 64)

// sourceFixture is a release directory of one signed core (0.2.0) and one
// signed gateway (0.1.136) release.
type sourceFixture struct {
	keys    map[string]ed25519.PublicKey
	priv    ed25519.PrivateKey
	id      string
	mu      sync.Mutex
	files   map[string][]byte // path below /rel/ -> content
	fail    bool
	srv     *httptest.Server
	core    rc.Manifest
	gateway rc.GatewayManifest
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &sourceFixture{keys: map[string]ed25519.PublicKey{"test": pub}, priv: priv, id: "src-" + hex.EncodeToString(pub[:8]), files: map[string][]byte{}}
	f.core = testRelease(f.id).Manifest
	f.core.CoreVersion = "0.2.0"
	f.core.Platforms = []rc.Platform{{OS: "linux", Arch: "amd64", RuntimeABI: "test", BundleDigest: bundleDigest, BundleBytes: 100,
		Files: []rc.File{{Path: "bin/sub2api", SHA256: strings.Repeat("b", 64), Size: 10, Mode: 0755}}}}
	f.gateway = rc.GatewayManifest{Kind: rc.GatewayManifestKind, GatewayVersion: "0.1.136", ReleaseID: f.id + "-gw", BuildID: f.id + "-gw",
		PeerProtocol: PeerProtocol, RuntimeProtocol: rc.Protocol,
		Platforms: []rc.GatewayPlatform{{OS: "linux", Arch: "amd64", Archive: "gw.tar.gz", ArchiveDigest: strings.Repeat("c", 64), ArchiveBytes: 50, BinaryDigest: strings.Repeat("d", 64)}},
		Images:    []rc.GatewayImage{{Ref: "ghcr.io/o/gw:0.1.136", Digest: "sha256:" + strings.Repeat("e", 64)}}}
	f.publish()
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// publish signs the manifests as they are now.
func (f *sourceFixture) publish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files["core/"+coreManifestAsset] = f.sign(f.core)
	f.files["core/"+bundleDigest+".tar.gz"] = bytes.Repeat([]byte("b"), 100)
	f.files["gateway/"+gatewayManifestAsset] = f.sign(f.gateway)
	f.files["gateway/gw.tar.gz"] = bytes.Repeat([]byte("g"), 50)
}

func (f *sourceFixture) sign(v any) []byte {
	payload, _ := json.Marshal(v)
	signed, _ := json.Marshal(rc.SignedManifest{KeyID: "test", Payload: payload, Signature: ed25519.Sign(f.priv, payload)})
	return signed
}

func (f *sourceFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.URL.Path == "/rel/releases.json" {
		releases := map[string]map[string]any{}
		for p, b := range f.files {
			line := path.Dir(p)
			if releases[line] == nil {
				version := map[string]string{"core": f.core.CoreVersion, "gateway": f.gateway.GatewayVersion}[line]
				releases[line] = map[string]any{"line": line, "version": version, "notes": line + " notes", "page": "https://releases.example.com/" + line,
					"published_at": "2026-11-20T08:00:00Z", "assets": map[string]any{}}
			}
			releases[line]["assets"].(map[string]any)[path.Base(p)] = map[string]any{"path": p, "size": len(b)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema": updatesource.IndexSchema, "releases": []any{releases["core"], releases["gateway"]}})
		return
	}
	b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/rel/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

func (f *sourceFixture) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, f.srv.Listener.Addr().String())
		},
	}}
}

func (f *sourceFixture) service(t *testing.T) *UpdateService {
	cipher, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	u := NewUpdateService(&release.Manager{TrustedKeys: f.keys, OS: "linux", Arch: "amd64", RuntimeABI: "test"}, cipher, false)
	u.Sources.Client = f.client()
	return u
}

// updateStore is a two-node cluster (on rolling-signed releases when rolling) whose
// update source is the fixture.
func updateStore(t *testing.T, rolling bool) (*Store, *sourceFixture) {
	t.Helper()
	s, _, _, _ := setupEnginesWith(t, rolling)
	f := newSourceFixture(t)
	s.Updates = f.service(t)
	if _, err := s.SetUpdateSource(context.Background(), SourceRequest{Enabled: true, Kind: "url", URL: fixtureURL}); err != nil {
		t.Fatal(err)
	}
	return s, f
}

// call sends one management request and decodes the answer.
func call(t *testing.T, s *Store, method, target string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, target, r))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %d %s", method, target, w.Code, w.Body.String())
	}
	return w.Code, out
}

func errorCode(out map[string]any) (code, field string) {
	e, _ := out["error"].(map[string]any)
	code, _ = e["code"].(string)
	if d, ok := e["details"].(map[string]any); ok {
		if fields, ok := d["fields"].([]any); ok && len(fields) > 0 {
			field, _ = fields[0].(map[string]any)["field"].(string)
		}
	}
	return
}

func TestPostgresUpdateSourceSettingsAndSecret(t *testing.T) {
	s, _, _, _ := setupEngines(t)
	s.Updates = newSourceFixture(t).service(t)
	ctx := context.Background()

	code, out := call(t, s, "GET", "/system/update-source", nil)
	data := out["data"].(map[string]any)
	if code != 200 || data["enabled"] != true || data["kind"] != "github" || data["repository"] != updatesource.DefaultRepository ||
		data["has_secret"] != false || data["default_repository"] != updatesource.DefaultRepository || data["url"] != "" {
		t.Fatalf("defaults: %d %v", code, out)
	}
	rev := data["revision"].(float64)

	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"enabled": true, "kind": "s3"}, "kind"},
		{map[string]any{"enabled": true, "kind": "github", "repository": "https://evil.test/o/r"}, "repository"},
		{map[string]any{"enabled": true, "kind": "url"}, "url"},
		{map[string]any{"enabled": true, "kind": "url", "url": "https://u:p@r.example.com/?token=x"}, "url"},
		{map[string]any{"enabled": true, "kind": "github", "repository": "o/r", "secret": "has space"}, "secret"},
	} {
		code, out = call(t, s, "PUT", "/system/update-source", tc.body)
		if c, f := errorCode(out); code != 400 || c != "invalid_argument" || f != tc.field {
			t.Fatalf("%v: %d %v", tc.body, code, out)
		}
		if strings.Contains(out["error"].(map[string]any)["message"].(string), "token=x") {
			t.Fatal("error echoes the URL query")
		}
	}

	cipher := s.Updates.Cipher
	s.Updates.Cipher = nil
	code, out = call(t, s, "PUT", "/system/update-source", map[string]any{"enabled": true, "kind": "github", "repository": "o/r", "secret": "tok-1"})
	if c, _ := errorCode(out); code != 400 || c != "master_key_missing" {
		t.Fatalf("no master key: %d %v", code, out)
	}
	s.Updates.Cipher = cipher

	// put stores body and checks what the answer says became of the secret.
	put := func(body map[string]any, changed bool, secret string) map[string]any {
		t.Helper()
		code, out := call(t, s, "PUT", "/system/update-source", body)
		if code != 200 || strings.Contains(stringify(out), "tok-") {
			t.Fatalf("%v: %d %v", body, code, out)
		}
		data := out["data"].(map[string]any)
		if data["changed"] != changed || data["secret_change"] != secret {
			t.Fatalf("%v: changed %v, secret %v", body, data["changed"], data["secret_change"])
		}
		return data
	}
	data = put(map[string]any{"enabled": true, "kind": "github", "repository": "https://github.com/o/r", "secret": "tok-1"}, true, "set")
	if data["has_secret"] != true || data["repository"] != "o/r" || data["revision"].(float64) != rev+1 {
		t.Fatalf("set secret: %v", data)
	}
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.secret, []byte("tok-1")) {
		t.Fatal("secret stored in plaintext")
	}
	if cfg, err := row.config(cipher); err != nil || cfg.Secret != "tok-1" {
		t.Fatalf("secret round trip: %q %v", cfg.Secret, err)
	}
	// The ciphertext is bound to its source: moved to another one it fails.
	moved := row
	moved.repo = "o/other"
	if _, err = moved.config(cipher); err == nil {
		t.Fatal("secret opened for another source")
	}
	if data = put(map[string]any{"enabled": true, "kind": "github", "repository": "o/r"}, false, "unchanged"); data["has_secret"] != true || data["revision"].(float64) != rev+1 {
		t.Fatalf("unchanged source: %v", data)
	}
	if data = put(map[string]any{"enabled": false, "kind": "github", "repository": "o/r"}, true, "unchanged"); data["has_secret"] != true || data["revision"].(float64) != rev+2 {
		t.Fatalf("switch off: %v", data)
	}
	// The url a github source does not read is no move.
	if data = put(map[string]any{"enabled": false, "kind": "github", "repository": "o/r", "url": fixtureURL}, true, "unchanged"); data["has_secret"] != true {
		t.Fatalf("github source with a url: %v", data)
	}
	// Moved without a secret field: the secret is dropped, and says so.
	if data = put(map[string]any{"enabled": true, "kind": "github", "repository": "o/r2"}, true, "cleared"); data["has_secret"] != false || data["revision"].(float64) != rev+4 {
		t.Fatalf("moved source kept its secret: %v", data)
	}
	put(map[string]any{"enabled": true, "kind": "url", "url": fixtureURL, "repository": "o/r", "secret": "tok-2"}, true, "set")
	// The repository a url source does not read is no move either.
	if data = put(map[string]any{"enabled": true, "kind": "url", "url": fixtureURL, "repository": ""}, true, "unchanged"); data["has_secret"] != true || data["repository"] != "" {
		t.Fatalf("url source without its old repository: %v", data)
	}
	if data = put(map[string]any{"enabled": true, "kind": "url", "url": fixtureURL, "secret": ""}, true, "cleared"); data["has_secret"] != false || data["url"] != fixtureURL {
		t.Fatalf("clear secret: %v", data)
	}
	put(map[string]any{"enabled": true, "kind": "url", "url": fixtureURL, "secret": ""}, false, "unchanged")
}

func stringify(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPostgresUpdateCheckPersistsAndKeepsLastSuccess(t *testing.T) {
	s, f := updateStore(t, true)
	ctx := context.Background()
	if err := s.CheckUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := s.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, g := first.Core, first.Gateway
	if !first.Enabled || first.Source.Kind != "url" || first.Source.URL != fixtureURL {
		t.Fatalf("source: %+v", first)
	}
	if c.CurrentVersion != "0.1.0-dev" || c.LatestVersion != "0.2.0" || !c.HasUpdate || !c.Compatible || c.Strategy != "rolling" || c.Tag != "core-v0.2.0" ||
		c.Error != "" || c.CheckedAt == nil || c.LastSuccessAt == nil || c.Page != "https://releases.example.com/core" || c.PublishedAt != "2026-11-20T08:00:00Z" {
		t.Fatalf("core: %+v", c)
	}
	if g.LatestVersion != "0.1.136" || !g.HasUpdate || !g.Compatible || g.SelfUpdate || g.Error != "" || len(g.Nodes) != 2 ||
		g.Image != "ghcr.io/o/gw:0.1.136@sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("gateway: %+v", g)
	}
	if due, err := s.updateCheckDue(ctx, updateCheckDebounce); err != nil || due {
		t.Fatalf("fresh results due again: %v %v", due, err)
	}

	// A failed check keeps the last success and says why.
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	if err = s.CheckUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := s.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fc := failed.Core; fc.LatestVersion != "0.2.0" || !fc.HasUpdate || fc.Error == "" || !fc.LastSuccessAt.Equal(*c.LastSuccessAt) || !fc.CheckedAt.After(*c.CheckedAt) {
		t.Fatalf("failed core check: %+v", fc)
	}
	if failed.Gateway.LatestVersion != "0.1.136" || failed.Gateway.Error == "" {
		t.Fatalf("failed gateway check: %+v", failed.Gateway)
	}

	// Results of another source are not shown, nor kept on failure.
	if _, err = s.SetUpdateSource(ctx, SourceRequest{Enabled: true, Kind: "url", URL: "https://other.example.com/rel/"}); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	other, err := s.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if other.Core.LatestVersion != "" || other.Core.HasUpdate || other.Core.Error == "" || other.Core.LastSuccessAt != nil {
		t.Fatalf("new source after failure: %+v", other.Core)
	}

	// An untrusted signature is an error naming the key.
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	if _, err = s.SetUpdateSource(ctx, SourceRequest{Enabled: true, Kind: "url", URL: fixtureURL}); err != nil {
		t.Fatal(err)
	}
	s.Updates.Manager.TrustedKeys = map[string]ed25519.PublicKey{}
	if err = s.CheckUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	untrusted, err := s.UpdateCheck(ctx)
	if err != nil || !strings.Contains(untrusted.Core.Error, `untrusted key "test"`) || !strings.Contains(untrusted.Gateway.Error, `untrusted key "test"`) {
		t.Fatalf("untrusted: %+v %v", untrusted, err)
	}
}

func TestPostgresUpdateCheckForceAndDisabled(t *testing.T) {
	s, _ := updateStore(t, true)
	code, out := call(t, s, "GET", "/system/update-check?force=true", nil)
	core := out["data"].(map[string]any)["core"].(map[string]any)
	if code != 200 || core["latest_version"] != "0.2.0" || core["has_update"] != true {
		t.Fatalf("forced check: %d %v", code, out)
	}
	if _, err := s.SetUpdateSource(context.Background(), SourceRequest{Enabled: false, Kind: "url", URL: fixtureURL}); err != nil {
		t.Fatal(err)
	}
	if due, err := s.updateCheckDue(context.Background(), 0); err != nil || due {
		t.Fatalf("disabled source due: %v %v", due, err)
	}
	code, out = call(t, s, "GET", "/system/update-check?force=true", nil)
	if data := out["data"].(map[string]any); code != 200 || data["enabled"] != false {
		t.Fatalf("disabled: %d %v", code, out)
	}
}

func TestPostgresGatewayVersionRegistered(t *testing.T) {
	s, a, _, _ := setupEngines(t)
	ctx := context.Background()
	n := a.Node
	n.GatewayVersion = "0.1.136"
	if err := s.Register(ctx, n); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range nodes {
		if want := map[bool]string{true: "0.1.136", false: ""}[got.ID == n.ID]; got.GatewayVersion != want {
			t.Fatalf("%s: gateway_version %q", got.ID, got.GatewayVersion)
		}
	}
}

func TestPostgresApplyCoreRolling(t *testing.T) {
	s, f := updateStore(t, true)
	ctx := context.Background()
	code, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "core", "version": "0.2.0", "idempotency_key": "k1"})
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "started" || data["plan_id"] == "" {
		t.Fatalf("apply: %d %v", code, out)
	}
	plan, err := s.Plan(ctx, data["plan_id"].(string))
	if err != nil || plan.Strategy != Rolling {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	r, err := s.Release(ctx, plan.ReleaseDigest)
	if err != nil || r.BundleBase != "" || r.SourceAssets[bundleDigest+".tar.gz"].Size != 100 || r.SourceLocator != "url:"+fixtureURL {
		t.Fatalf("imported release: %+v %v", r, err)
	}
	code, again := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "core", "version": "0.2.0", "idempotency_key": "k1"})
	if code != 200 || again["data"].(map[string]any)["plan_id"] != data["plan_id"] {
		t.Fatalf("idempotent apply: %d %v", code, again)
	}
	code, out = call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "core", "version": "0.2.0"})
	if c, _ := errorCode(out); code != 409 || c != "preflight_blocked" || !strings.Contains(stringify(out), "another upgrade is active") {
		t.Fatalf("second plan: %d %v", code, out)
	}
	details := out["error"].(map[string]any)["details"].(map[string]any)
	if codes := stringify(details["blocker_codes"]); codes != `[{"code":"upgrade_active","message":"another upgrade is active"}]` ||
		stringify(details["preflight"].(map[string]any)["blocker_codes"]) != codes {
		t.Fatalf("blocker codes: %v", details)
	}

	// Bundles come through the source the release was imported from.
	body, err := s.OpenSourceAsset(ctx, r, bundleDigest+".tar.gz", 100)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, f.files["core/"+bundleDigest+".tar.gz"]) {
		t.Fatalf("bundle %q", got)
	}
	// A new secret or the switch leaves the running plan's downloads alone;
	// another url does not get the release's references, nor the secret.
	for _, q := range []SourceRequest{{Enabled: true, Kind: "url", URL: fixtureURL, Secret: ptr("tok-3")}, {Enabled: false, Kind: "url", URL: fixtureURL}} {
		if _, err = s.SetUpdateSource(ctx, q); err != nil {
			t.Fatal(err)
		}
		body, err := s.OpenSourceAsset(ctx, r, bundleDigest+".tar.gz", 100)
		if err != nil {
			t.Fatalf("asset after %+v: %v", q, err)
		}
		body.Close()
	}
	if _, err = s.SetUpdateSource(ctx, SourceRequest{Enabled: true, Kind: "url", URL: "https://other.example.com/rel/"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenSourceAsset(ctx, r, bundleDigest+".tar.gz", 100); err == nil || !strings.Contains(err.Error(), "apply it again") {
		t.Fatalf("asset of a moved source: %v", err)
	}
}

// TestPostgresSourceChangeWakesTheCheckLoop: a source stored through any node
// is checked at once by that node's RunUpdateChecks, which ends with it.
func TestPostgresSourceChangeWakesTheCheckLoop(t *testing.T) {
	s, _ := updateStore(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunUpdateChecks(ctx, "not-the-primary"); close(done) }()
	defer func() {
		cancel()
		<-done
	}()
	if _, err := s.SetUpdateSource(ctx, SourceRequest{Enabled: true, Kind: "url", URL: fixtureURL, Secret: ptr("tok-4")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := s.UpdateCheck(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.Core.LatestVersion == "0.2.0" && c.Gateway.LatestVersion == "0.1.136" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new source was not checked: %+v", c)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ptr[T any](v T) *T { return &v }

func TestPostgresApplyCorePrimaryFirstNeedsConfirmation(t *testing.T) {
	s, f := updateStore(t, false)
	f.core.Strategy = "maintenance"
	f.publish()
	code, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "core", "version": "0.2.0"})
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "needs_confirmation" || data["preflight"] == nil {
		t.Fatalf("unconfirmed: %d %v", code, out)
	}
	reasons, codes := data["reasons"].([]any), data["reason_codes"].([]any)
	if len(codes) != len(reasons) || len(codes) == 0 {
		t.Fatalf("reason codes %v for %v", codes, reasons)
	}
	for i, c := range codes {
		c := c.(map[string]any)
		if c["message"] != reasons[i] || (c["code"] == issueTooFewServing) != (c["params"] != nil) {
			t.Fatalf("reason code %v for %q", c, reasons[i])
		}
	}
	if plans, err := s.Plans(context.Background()); err != nil || len(plans) != 0 {
		t.Fatalf("plan created without confirmation: %d %v", len(plans), err)
	}
	code, out = call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "core", "version": "0.2.0", "confirm": true})
	if data, _ = out["data"].(map[string]any); code != 200 || data["status"] != "started" {
		t.Fatalf("confirmed: %d %v", code, out)
	}
}

func TestPostgresApplyRejects(t *testing.T) {
	s, _ := updateStore(t, true)
	for _, tc := range []struct {
		body        map[string]any
		status      int
		code, field string
	}{
		{map[string]any{"line": "plugins", "version": "1.0.0"}, 400, "invalid_argument", "line"},
		{map[string]any{"line": "core", "version": "v0.2.0"}, 400, "invalid_argument", "version"},
		{map[string]any{"line": "core", "version": "0.2.1-rc.1"}, 400, "invalid_argument", "version"},
		{map[string]any{"line": "core", "version": "0.9.0"}, 404, "release_not_found", ""},
	} {
		code, out := call(t, s, "POST", "/system/updates/apply", tc.body)
		if c, f := errorCode(out); code != tc.status || c != tc.code || f != tc.field {
			t.Fatalf("%v: %d %v", tc.body, code, out)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/system/releases/import", strings.NewReader(`{"repository":"o/r","tag":"v1.0.0"}`)))
	if w.Code != 404 && w.Code != 405 {
		t.Fatalf("old import route still served: %d", w.Code)
	}
}

// memSource is an in-memory update source.
type memSource struct{ files map[string][]byte }

func (m memSource) Releases(context.Context) ([]updatesource.Listing, error) { return nil, nil }
func (m memSource) Open(_ context.Context, a updatesource.Asset, max int64) (io.ReadCloser, error) {
	b, ok := m.files[a.Ref]
	if !ok || int64(len(b)) > max {
		return nil, errors.New("no such asset")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// listing describes line of f as an index would.
func (f *sourceFixture) listing(line, version string) (updatesource.Listing, memSource) {
	l := updatesource.Listing{Line: line, Version: version, Tag: line + "-v" + version, Assets: map[string]updatesource.Asset{}}
	src := memSource{files: map[string][]byte{}}
	for p, b := range f.files {
		if path.Dir(p) == line {
			l.Assets[path.Base(p)] = updatesource.Asset{Name: path.Base(p), Size: int64(len(b)), Ref: p}
			src.files[p] = b
		}
	}
	return l, src
}

func TestVerifyCoreRequiresSignatureAndMatchingAssets(t *testing.T) {
	f := newSourceFixture(t)
	u := f.service(t)
	ctx := context.Background()
	l, src := f.listing("core", "0.2.0")
	if _, err := u.verifyCore(ctx, src, l); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(l *updatesource.Listing, src memSource){
		"missing manifest": func(l *updatesource.Listing, _ memSource) { delete(l.Assets, coreManifestAsset) },
		"missing bundle":   func(l *updatesource.Listing, _ memSource) { delete(l.Assets, bundleDigest+".tar.gz") },
		"wrong size": func(l *updatesource.Listing, _ memSource) {
			a := l.Assets[bundleDigest+".tar.gz"]
			a.Size++
			l.Assets[a.Name] = a
		},
		"wrong tag": func(l *updatesource.Listing, _ memSource) { l.Version = "0.3.0" },
		"bad signature": func(l *updatesource.Listing, src memSource) {
			var s rc.SignedManifest
			_ = json.Unmarshal(src.files["core/"+coreManifestAsset], &s)
			s.Signature[0] ^= 1
			src.files["core/"+coreManifestAsset], _ = json.Marshal(s)
		},
		"gateway manifest": func(l *updatesource.Listing, src memSource) {
			src.files["core/"+coreManifestAsset] = f.files["gateway/"+gatewayManifestAsset]
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, src := f.listing("core", "0.2.0")
			mutate(&l, src)
			if _, err := u.verifyCore(ctx, src, l); err == nil {
				t.Fatal("unsafe core release accepted")
			}
		})
	}
}

func TestVerifyGatewayRequiresKindVersionAndArchives(t *testing.T) {
	f := newSourceFixture(t)
	u := f.service(t)
	ctx := context.Background()
	l, src := f.listing("gateway", "0.1.136")
	if _, err := u.verifyGateway(ctx, src, l); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(l *updatesource.Listing, src memSource){
		"core manifest": func(_ *updatesource.Listing, src memSource) {
			src.files["gateway/"+gatewayManifestAsset] = f.files["core/"+coreManifestAsset]
		},
		"wrong tag":       func(l *updatesource.Listing, _ memSource) { l.Version = "0.1.137" },
		"missing archive": func(l *updatesource.Listing, _ memSource) { delete(l.Assets, "gw.tar.gz") },
		"wrong kind": func(_ *updatesource.Listing, src memSource) {
			m := f.gateway
			m.Kind = "sub2api-core"
			src.files["gateway/"+gatewayManifestAsset] = f.sign(m)
		},
		"trailing data": func(_ *updatesource.Listing, src memSource) {
			b, _ := json.Marshal(f.gateway)
			payload := append(b, []byte(` {}`)...)
			src.files["gateway/"+gatewayManifestAsset], _ = json.Marshal(rc.SignedManifest{KeyID: "test", Payload: payload, Signature: ed25519.Sign(f.priv, payload)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, src := f.listing("gateway", "0.1.136")
			mutate(&l, src)
			if _, err := u.verifyGateway(ctx, src, l); err == nil {
				t.Fatal("unsafe gateway release accepted")
			}
		})
	}
}

func TestGatewayCompatibility(t *testing.T) {
	f := newSourceFixture(t)
	current := testRelease("current")
	nodes := []Node{{ID: "a", Enabled: true, OS: "linux", Arch: "amd64"}, {ID: "b", Enabled: false, OS: "linux", Arch: "arm64"}}
	if got := gatewayCompatibility(f.gateway, current, nodes); len(got) != 0 {
		t.Fatal(got)
	}
	m := f.gateway
	m.PeerProtocol, m.RuntimeProtocol = PeerProtocol+1, rc.Protocol+1
	nodes[1].Enabled = true
	if got := strings.Join(gatewayCompatibility(m, current, nodes), "; "); !strings.Contains(got, "peer protocol") || !strings.Contains(got, "runtime protocol") || !strings.Contains(got, "node b") {
		t.Fatal(got)
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		version, latest string
		want            bool
	}{{"0.1.0", "0.2.0", true}, {"0.2.0", "0.2.0", false}, {"0.3.0", "0.2.0", false}, {"", "0.2.0", true}, {"0.0.0-dev", "0.1.0", true}, {"0.1.0", "", false}} {
		if got := older(c.version, c.latest); got != c.want {
			t.Errorf("older(%q, %q) = %v", c.version, c.latest, got)
		}
	}
}

func TestLocalRuntimePreparesSourceReleasesThroughTheSource(t *testing.T) {
	f := newSourceFixture(t)
	var signed rc.SignedManifest
	_ = json.Unmarshal(f.files["core/"+coreManifestAsset], &signed)
	r := Release{Digest: release.Digest(signed.Payload), Manifest: f.core, Signed: signed}
	rt := &LocalRuntime{Releases: &release.Manager{Root: t.TempDir(), TrustedKeys: f.keys, OS: "linux", Arch: "amd64", RuntimeABI: "test"}}
	if err := rt.Prepare(context.Background(), r, ""); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("no source: %v", err)
	}
	var asked string
	var max int64
	rt.SourceAsset = func(_ context.Context, _ Release, name string, limit int64) (io.ReadCloser, error) {
		asked, max = name, limit
		return nil, errors.New("offline")
	}
	if err := rt.Prepare(context.Background(), r, ""); err == nil || asked != bundleDigest+".tar.gz" || max != 100 {
		t.Fatalf("source asset %q (%d): %v", asked, max, err)
	}
}
