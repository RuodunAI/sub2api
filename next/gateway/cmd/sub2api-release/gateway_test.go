package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/control"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// writeArchive writes a tar.gz of the given entries (name -> content; a
// trailing "/" makes a directory) and returns its path.
func writeArchive(t *testing.T, path string, entries ...[2]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Mode: 0755, Size: int64(len(e[1])), Typeflag: tar.TypeReg}
		if strings.HasSuffix(e[0], "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil && h.Typeflag == tar.TypeReg {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifyGateway is the gateway-side check of DESIGN-2026-11-17 §3: the
// signature over the exact payload, a strict decode and the manifest kind.
func verifyGateway(t *testing.T, raw []byte, keys map[string]ed25519.PublicKey) rc.GatewayManifest {
	t.Helper()
	var signed rc.SignedManifest
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(keys[signed.KeyID], signed.Payload, signed.Signature) {
		t.Fatal("gateway manifest signature does not verify")
	}
	var m rc.GatewayManifest
	dec := json.NewDecoder(bytes.NewReader(signed.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Kind != rc.GatewayManifestKind {
		t.Fatalf("kind %q", m.Kind)
	}
	return m
}

func TestPackGatewaySignsArchivesAndImages(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "keys")
	if err := run([]string{"keygen", "--out", keys}, io.Discard); err != nil {
		t.Fatal(err)
	}
	bins := map[string]string{"amd64": "gateway amd64", "arm64": "gateway arm64"}
	args := []string{"pack-gateway", "--out", filepath.Join(root, "out"), "--key", filepath.Join(keys, "release.key"), "--key-id", "test",
		"--version", "0.1.136", "--source-commit", "abc", "--build-id", "gh-1-1",
		"--image", "ghcr.io/owner/sup2api-gateway:0.1.136@sha256:" + strings.Repeat("a", 64)}
	for arch, bin := range bins {
		path := writeArchive(t, filepath.Join(root, "sub2api-gateway_0.1.136_linux_"+arch+".tar.gz"), [2]string{"sub2api-gateway", bin})
		args = append(args, "--archive", "linux/"+arch+"="+path)
	}
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "out", gatewayManifestAsset))
	if err != nil {
		t.Fatal(err)
	}
	m := verifyGateway(t, raw, map[string]ed25519.PublicKey{"test": readPub(t, filepath.Join(keys, "release.pub"))})
	if m.GatewayVersion != "0.1.136" || m.ReleaseID != "gateway-v0.1.136" || m.BuildID != "gh-1-1" || m.SourceCommit != "abc" || m.CreatedAt.IsZero() ||
		m.PeerProtocol != control.PeerProtocol || m.RuntimeProtocol != rc.Protocol || len(m.Platforms) != 2 {
		t.Fatalf("manifest %+v", m)
	}
	if len(m.Images) != 1 || m.Images[0].Ref != "ghcr.io/owner/sup2api-gateway:0.1.136" || m.Images[0].Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("images %+v", m.Images)
	}
	for _, p := range m.Platforms {
		digest, size, err := fileDigest(filepath.Join(root, p.Archive))
		bin := sha256.Sum256([]byte(bins[p.Arch]))
		if err != nil || p.OS != "linux" || p.ArchiveDigest != digest || p.ArchiveBytes != size || p.BinaryDigest != hex.EncodeToString(bin[:]) {
			t.Fatalf("platform %+v: %v", p, err)
		}
	}
	if !strings.HasPrefix(out.String(), "manifest_digest=") {
		t.Fatalf("output %q", out.String())
	}
	if err := run(args, io.Discard); err == nil {
		t.Fatal("overwrote an existing manifest")
	}
}

func TestPackGatewayRejectsBadInputs(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "keys")
	if err := run([]string{"keygen", "--out", keys}, io.Discard); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "sub2api-gateway_0.1.136_linux_amd64.tar.gz")
	cases := map[string]struct {
		archive []string
		extra   []string
	}{
		"extra file":     {archive: []string{"sub2api-gateway", "x", "README", "y"}},
		"wrong name":     {archive: []string{"bin/sub2api-gateway", "x"}},
		"directory":      {archive: []string{"sub2api-gateway/", ""}},
		"empty binary":   {archive: []string{"sub2api-gateway", ""}},
		"bad image":      {archive: []string{"sub2api-gateway", "x"}, extra: []string{"--image", "ghcr.io/o/i:1"}},
		"prerelease":     {archive: []string{"sub2api-gateway", "x"}, extra: []string{"--version", "0.1.136-rc.1"}},
		"prefixed":       {archive: []string{"sub2api-gateway", "x"}, extra: []string{"--version", "v0.1.136"}},
		"archive naming": {archive: []string{"sub2api-gateway", "x"}, extra: []string{"--version", "0.1.137"}},
	}
	for label, c := range cases {
		var entries [][2]string
		for i := 0; i < len(c.archive); i += 2 {
			entries = append(entries, [2]string{c.archive[i], c.archive[i+1]})
		}
		writeArchive(t, name, entries...)
		args := []string{"pack-gateway", "--out", filepath.Join(root, "out-"+strings.ReplaceAll(label, " ", "-")), "--key", filepath.Join(keys, "release.key"), "--key-id", "test",
			"--version", "0.1.136", "--source-commit", "abc", "--archive", "linux/amd64=" + name}
		if err := run(append(args, c.extra...), io.Discard); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}
