package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
	"gorm.io/gorm"
)

// freshDatabase creates an empty database beside TEST_DATABASE_URL, dropped
// when the test ends, and opens it.
func freshDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	admin := openTestDB(t, dsn)
	name := fmt.Sprintf("shell_schema_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE DATABASE " + quoteIdent(name)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE " + quoteIdent(name) + " WITH (FORCE)") })
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return openTestDB(t, u.String())
	}
	return openTestDB(t, dsn+" dbname="+name)
}

// coreBundle is a release bundle holding only bin/sub2api, and its file list.
func coreBundle(t *testing.T) ([]byte, []rc.File) {
	t.Helper()
	body := []byte("#!/bin/sh\nexit 0\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/sub2api", Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes(), []rc.File{{Path: "bin/sub2api", SHA256: release.Digest(body), Size: int64(len(body)), Mode: 0755}}
}

// TestSchemaKeepsSignedReleasesAsBaseline starts from the database gateway
// 0.1.135 leaves: its schema, a release stored from a signed manifest (as
// core 0.1.291 is in production) and a cluster whose baseline it is. After
// EnsureSchema the release keeps its digest, its stored manifest is the
// payload that digest covers, it installs and switches as the baseline, and
// an unsigned release can follow it.
func TestSchemaKeepsSignedReleasesAsBaseline(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()
	cut := strings.Index(schema, "-- Unsigned releases")
	if cut < 0 || !strings.Contains(schema[:cut], "signed_manifest jsonb NOT NULL") {
		t.Fatal("cannot find the schema of gateway 0.1.135 in schema.sql")
	}
	if err := execLocked(db, schema[:cut]); err != nil {
		t.Fatal(err)
	}

	bundle, files := coreBundle(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bundle) }))
	defer srv.Close()
	m := testRelease("0.1.291").Manifest
	m.CoreVersion, m.SourceCommit = "0.1.291", "a0eac3c38"
	m.Platforms = []rc.Platform{{OS: "linux", Arch: "amd64", RuntimeABI: "test", BundleDigest: release.Digest(bundle), BundleBytes: int64(len(bundle)), Files: files}}
	// Gateway 0.1.135 stored the decoded manifest as jsonb and the envelope
	// whose payload (base64 in JSON) is the exact signed bytes.
	payload, _ := json.Marshal(m)
	envelope, _ := json.Marshal(map[string]any{"key_id": "sup2api-ovh-2026", "payload": payload, "signature": bytes.Repeat([]byte{1}, 64)})
	digest := release.Digest(payload)
	if err := db.Exec(`INSERT INTO updater.releases (digest,release_id,manifest,signed_manifest,bundle_base) VALUES (?,?,?::jsonb,?::jsonb,?)`,
		digest, m.ReleaseID, string(payload), string(envelope), srv.URL).Error; err != nil {
		t.Fatal(err)
	}
	// A row without a payload, as tests of older gateways stored, must not
	// stop the conversion.
	if err := db.Exec(`INSERT INTO updater.releases (digest,release_id,manifest,signed_manifest,bundle_base) VALUES (?,?,?::jsonb,?::jsonb,'')`,
		strings.Repeat("0", 64), "unverified", `{"release_id":"unverified"}`, `{"key_id":"","payload":null,"signature":null}`).Error; err != nil {
		t.Fatal(err)
	}
	s := &Store{DB: db, Cluster: "production"}
	if err := s.InitCluster(ctx, "ovh", digest); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(ctx); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	_, baseline, _, err := s.ClusterState(ctx)
	if err != nil || baseline != digest {
		t.Fatalf("baseline %s: %v", baseline, err)
	}
	r, err := s.Release(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Raw, payload) || release.Digest(r.Raw) != digest || r.Manifest.CoreVersion != "0.1.291" || r.BundleBase != srv.URL {
		t.Fatalf("converted release: %+v (raw %q)", r, r.Raw)
	}
	// The envelope stays for gateway 0.1.135, which reads it.
	var kept string
	if err = releaseRow(db, "core", digest).Select("signed_manifest->>'payload'").Row().Scan(&kept); err != nil || kept != base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("envelope left for gateway 0.1.135: %q %v", kept, err)
	}

	rt := &LocalRuntime{Releases: &release.Manager{Root: t.TempDir(), OS: "linux", Arch: "amd64", RuntimeABI: "test", AllowHTTP: true}}
	if err = rt.Prepare(ctx, r, r.BundleBase); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if err = rt.Releases.Switch(ctx, "start-"+digest, digest); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(filepath.Join(rt.Releases.Root, "manifests", digest)); err != nil || !bytes.Equal(raw, payload) {
			t.Fatalf("installed manifest %q: %v", raw, err)
		}
	}

	// An unsigned release of the next version is stored beside it and can
	// follow it.
	next := m
	next.CoreVersion, next.ReleaseID, next.BuildID = "0.1.292", "core-v0.1.292", "github-actions-1-1"
	raw, _ := json.MarshalIndent(next, "", "  ")
	target := Release{Digest: release.Digest(raw), Manifest: next, Raw: raw, BundleBase: srv.URL}
	if err = s.PutRelease(ctx, target); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Release(ctx, target.Digest); err != nil || !bytes.Equal(got.Raw, raw) {
		t.Fatalf("unsigned release: %v", err)
	}
	if blockers := Compatibility(r, target); len(blockers) != 0 {
		t.Fatalf("0.1.292 cannot follow the converted baseline: %v", blockers)
	}
}

// TestSchemaLockSpansScript proves that EnsureSchema's lock and script still
// run as one implicit transaction through GORM and database/sql: the script
// runs with the lock held, waits while another session holds it, and a
// failing statement undoes the ones before it.
func TestSchemaLockSpansScript(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	probe := quoteIdent(fmt.Sprintf("schema_lock_probe_%d", time.Now().UnixNano()))
	if err := s.db(ctx).Exec("CREATE TABLE " + probe + " (step text, pid int, held bigint)").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db(context.Background()).Exec("DROP TABLE IF EXISTS " + probe) })
	// record notes how many advisory locks the session running the script
	// holds at that point; a transaction-scoped lock is gone once the
	// transaction that took it ends.
	record := func(step string) string {
		return fmt.Sprintf("INSERT INTO %s SELECT '%s', pg_backend_pid(), count(*) FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted;\n", probe, step)
	}
	held := func(step string) (pid int, locks int64, rows int64) {
		t.Helper()
		if err := s.db(ctx).Raw("SELECT COALESCE(max(pid),0), COALESCE(max(held),0), count(*) FROM "+probe+" WHERE step=?", step).Row().Scan(&pid, &locks, &rows); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("lock held through the script", func(t *testing.T) {
		if err := execLocked(s.db(ctx), record("held")); err != nil {
			t.Fatal(err)
		}
		pid, locks, rows := held("held")
		if rows != 1 || locks != 1 {
			t.Fatalf("script ran with %d advisory locks in %d rows, want the schema lock", locks, rows)
		}
		var left int64
		if err := s.db(ctx).Raw("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=?", pid).Row().Scan(&left); err != nil || left != 0 {
			t.Fatalf("schema lock outlived the script: %d locks, %v", left, err)
		}
	})

	t.Run("script waits for the lock", func(t *testing.T) {
		holder := s.db(ctx).Begin()
		if holder.Error != nil {
			t.Fatal(holder.Error)
		}
		defer holder.Rollback()
		if err := holder.Exec("SELECT pg_advisory_xact_lock(hashtext('sub2api-shell-schema'))").Error; err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- execLocked(s.db(ctx), record("waited")) }()
		select {
		case err := <-done:
			t.Fatalf("script ran while another session held the schema lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if _, _, rows := held("waited"); rows != 0 {
			t.Fatal("script statements ran before the lock was granted")
		}
		if err := holder.Commit().Error; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, locks, rows := held("waited"); rows != 1 || locks != 1 {
			t.Fatalf("script after the wait: %d rows, %d locks", rows, locks)
		}
	})

	t.Run("failure undoes the script", func(t *testing.T) {
		if err := execLocked(s.db(ctx), record("undone")+"SELECT 1/0;"); err == nil {
			t.Fatal("failing script reported success")
		}
		if _, _, rows := held("undone"); rows != 0 {
			t.Fatal("statements before the failure were committed on their own")
		}
	})
}

// TestEnsureSchemaIsIdempotent applies the schema again over the cluster row
// of an older shell and checks the update-source defaults it fills in.
func TestEnsureSchemaIsIdempotent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.InitCluster(ctx, "a", "base"); err != nil {
		t.Fatal(err)
	}
	if err := s.clusterRow(s.db(ctx)).Update("update_repository", "").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(ctx); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		t.Fatal(err)
	}
	if !row.enabled || row.kind != "github" || row.repo != "Sub2API-Devs/sup2api" || row.url != "" || row.secret != nil {
		t.Fatalf("update-source defaults: %+v", row)
	}
	// A url source keeps the repository it was given, and its revision.
	if err = s.clusterRow(s.db(ctx)).Updates(map[string]any{"update_source_kind": "url", "update_url": "https://r.example.com/", "update_repository": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := s.readSource(s.clusterRow(s.db(ctx))); err != nil || again.repo != "" || again.revision != row.revision {
		t.Fatalf("url source after a restart: %+v %v", again, err)
	}
}

// EnsureSchema drops updater.nodes.cpu_percent, which only shells before
// 0.1.134 used.
func TestSchemaDropsNodeCPUPercent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.db(ctx).Exec("ALTER TABLE updater.nodes ADD COLUMN IF NOT EXISTS cpu_percent real").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := s.db(ctx).Raw("SELECT count(*) FROM information_schema.columns WHERE table_schema='updater' AND table_name='nodes' AND column_name='cpu_percent'").Row().Scan(&left); err != nil || left != 0 {
		t.Fatalf("cpu_percent left: %d %v", left, err)
	}
}
