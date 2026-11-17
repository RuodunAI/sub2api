package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// readManifest reads the one manifest pack wrote to dest and verifies it as
// a gateway does: its file name is the SHA-256 of its bytes.
func readManifest(t *testing.T, dest string) ([]byte, rc.Manifest) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dest, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("manifests %v: %v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	m, digest, err := release.Verify(raw)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(paths[0]) != digest+".json" {
		t.Fatalf("manifest %s has digest %s", paths[0], digest)
	}
	return raw, m
}

func TestPackImmutableRelease(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	dest := filepath.Join(root, "publish")
	if err := os.MkdirAll(filepath.Join(stage, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	core := []byte("real build output goes here\n")
	if err := os.WriteFile(filepath.Join(stage, "bin", "sub2api"), core, 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"pack", "--dir", stage, "--out", dest, "--release-id", "r1", "--core-version", "0.1.0", "--source-commit", "source-snapshot", "--schema", "schema-hash", "--os", "linux", "--arch", "amd64"}, &out); err != nil {
		t.Fatal(err)
	}
	raw, m := readManifest(t, dest)
	if m.ReleaseID != "r1" || m.SchemaBefore != "schema-hash" || m.SchemaAfter != "schema-hash" || m.Strategy != "rolling" {
		t.Fatalf("bad manifest: %+v", m)
	}
	if sum := sha256.Sum256(raw); !strings.HasPrefix(out.String(), "manifest_digest="+hex.EncodeToString(sum[:])+"\n") {
		t.Fatalf("output %q does not name the manifest file's digest", out.String())
	}
	if !m.ShellProtocol.Contains(rc.Protocol) || !m.CoreControlProtocol.Contains(rc.Protocol) {
		t.Fatal("release cannot be used by the current shell/core protocol")
	}
	p := m.Platforms[0]
	blob, err := os.ReadFile(filepath.Join(dest, p.BundleDigest+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(blob)
	if hex.EncodeToString(hash[:]) != p.BundleDigest || int64(len(blob)) != p.BundleBytes {
		t.Fatal("bundle digest/size mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "bin/sub2api" || h.Mode != 0755 || !bytes.Equal(b, core) {
		t.Fatal("archive mismatch")
	}
	if _, err = tr.Next(); err != io.EOF {
		t.Fatalf("extra archive entry: %v", err)
	}
	// A migration release must declare both ends. The source must not be
	// silently overwritten with the target contract when publishing the bundle.
	migrationDest := filepath.Join(root, "migration")
	if err = run([]string{"pack", "--dir", stage, "--out", migrationDest, "--release-id", "r2", "--core-version", "0.2.0", "--source-commit", "migration-snapshot", "--schema-before", "schema-hash", "--schema", "schema-new", "--os", "linux", "--arch", "amd64"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, migration := readManifest(t, migrationDest); migration.SchemaBefore != "schema-hash" || migration.SchemaAfter != "schema-new" || migration.Strategy != "maintenance" {
		t.Fatalf("migration contract lost: %+v", migration)
	}
}

func TestBundleRejectsMissingCoreAndSymlink(t *testing.T) {
	root := t.TempDir()
	if _, err := bundle(root, io.Discard); err == nil {
		t.Fatal("accepted empty release")
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "sub2api"), []byte("core"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "bin", "sub2api"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := bundle(root, io.Discard); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestPublishCannotReplaceArtifact(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(a, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishFile(a, target); err != nil {
		t.Fatal(err)
	}
	if err := publishFile(a, target); err != nil {
		t.Fatalf("same content should be idempotent: %v", err)
	}
	if err := publishFile(b, target); err == nil {
		t.Fatal("replaced existing artifact")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Fatal("existing release was modified")
	}
}

func TestReleaseStrategyFollowsTheSchema(t *testing.T) {
	if releaseStrategy("s1", "s1") != "rolling" {
		t.Fatal("a release that keeps the schema is declared rolling")
	}
	for _, c := range [][2]string{{"s1", "s2"}, {"", "s1"}, {"", ""}} {
		if releaseStrategy(c[0], c[1]) != "maintenance" {
			t.Fatalf("%q -> %q must be maintenance", c[0], c[1])
		}
	}
}

func TestPackDeclaresEveryPlatformInOneManifest(t *testing.T) {
	root := t.TempDir()
	args := []string{"pack", "--out", filepath.Join(root, "publish"), "--release-id", "core-v0.2.0", "--core-version", "0.2.0", "--source-commit", "c", "--schema", "s"}
	for _, arch := range []string{"amd64", "arm64"} {
		stage := filepath.Join(root, "stage-"+arch)
		if err := os.MkdirAll(filepath.Join(stage, "bin"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, "bin", "sub2api"), []byte("core "+arch), 0755); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--platform", "linux/"+arch+"="+stage)
	}
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	_, m := readManifest(t, filepath.Join(root, "publish"))
	if len(m.Platforms) != 2 || m.Platforms[0].Arch != "amd64" || m.Platforms[1].Arch != "arm64" || m.Platforms[0].BundleDigest == m.Platforms[1].BundleDigest {
		t.Fatalf("platforms: %+v", m.Platforms)
	}
	for _, p := range m.Platforms {
		if p.OS != "linux" || p.RuntimeABI != "linux-static-v1" {
			t.Fatalf("platform %+v", p)
		}
		digest, size, err := fileDigest(filepath.Join(root, "publish", p.BundleDigest+".tar.gz"))
		if err != nil || digest != p.BundleDigest || size != p.BundleBytes {
			t.Fatalf("bundle %s: %v", p.Arch, err)
		}
		if !strings.Contains(out.String(), "bundle_digest="+p.BundleDigest+"\n") {
			t.Fatalf("output does not name bundle %s: %s", p.BundleDigest, out.String())
		}
	}
	dup := append(args[:len(args):len(args)], "--platform", "linux/amd64="+root)
	if err := run(dup, io.Discard); err == nil {
		t.Fatal("accepted a platform twice")
	}
}

// pubkey prints builtin/trust.pub from a plugin signing key: the 32-byte seed
// sub2api-plugin keygen stores, or a 64-byte private key.
func TestPubkeyReadsPluginKeys(t *testing.T) {
	root := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(pub) + "\n"
	key := filepath.Join(root, "plugin.key")
	for _, b := range [][]byte{priv.Seed(), priv} {
		if err = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(b)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = run([]string{"pubkey", "--key", key}, &out); err != nil || out.String() != want {
			t.Fatalf("pubkey of a %d-byte key %q: %v", len(b), out.String(), err)
		}
	}
	if err = os.WriteFile(key, []byte("bm90IGEga2V5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"pubkey", "--key", key}, io.Discard); err == nil {
		t.Fatal("accepted an invalid key")
	}
}
