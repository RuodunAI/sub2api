package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// gatewayScript is a gateway binary stand-in: it answers "version".
func gatewayScript(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// tarGz packs entries in order.
func tarGz(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gatewayArchive is a valid archive of bin.
func gatewayArchive(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "sub2api-gateway", Mode: 0755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(bin)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// installFixture is a gateway release (its manifest bytes) of version with
// its archive.
type installFixture struct {
	m        *Manager
	manifest []byte
	archive  []byte
	bin      []byte
	opened   int
}

func newInstallFixture(t *testing.T, version string, mutate func(*rc.GatewayManifest, *[]byte)) *installFixture {
	t.Helper()
	m := gatewayFixture()
	f := &installFixture{m: &Manager{Root: t.TempDir(), OS: "linux", Arch: "amd64"}, bin: gatewayScript(version)}
	f.archive = gatewayArchive(t, f.bin)
	m.GatewayVersion = version
	m.Platforms[0].ArchiveDigest, m.Platforms[0].ArchiveBytes, m.Platforms[0].BinaryDigest = sha(f.archive), int64(len(f.archive)), sha(f.bin)
	if mutate != nil {
		mutate(&m, &f.archive)
	}
	f.manifest, _ = json.Marshal(m)
	return f
}

func (f *installFixture) open(_ context.Context, archive string, size int64) (io.ReadCloser, error) {
	f.opened++
	if archive != "sub2api-gateway_0.1.136_linux_amd64.tar.gz" || size != int64(len(f.archive)) {
		return nil, errors.New("unexpected archive request")
	}
	return io.NopCloser(bytes.NewReader(f.archive)), nil
}

func (f *installFixture) install() error {
	_, _, err := f.m.InstallGateway(context.Background(), f.manifest, f.open)
	return err
}

func TestInstallGatewayInstallsTheVerifiedBinary(t *testing.T) {
	f := newInstallFixture(t, "0.1.136", nil)
	if err := f.install(); err != nil {
		t.Fatal(err)
	}
	bin := f.m.GatewayBinary("0.1.136")
	st, err := os.Lstat(bin)
	if err != nil || st.Mode().Perm() != 0755 || !st.Mode().IsRegular() {
		t.Fatalf("installed binary: %v %v", st, err)
	}
	if got, _ := os.ReadFile(bin); !bytes.Equal(got, f.bin) {
		t.Fatalf("installed %q", got)
	}
	if err = f.install(); err != nil || f.opened != 1 {
		t.Fatalf("an intact installation is kept: %v (opened %d)", err, f.opened)
	}
	// A damaged installation is replaced.
	if err = os.WriteFile(bin, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = f.install(); err != nil || f.opened != 2 {
		t.Fatalf("reinstall: %v (opened %d)", err, f.opened)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.m.Root, "gateway")); len(entries) != 1 {
		t.Fatalf("staging left behind: %v", entries)
	}
}

func TestInstallGatewayRejectsUnsafeArchives(t *testing.T) {
	reg := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Mode: 0755, Size: size, Typeflag: tar.TypeReg}
	}
	cases := map[string]func(t *testing.T, m *rc.GatewayManifest, archive *[]byte){
		"archive digest": func(_ *testing.T, m *rc.GatewayManifest, _ *[]byte) {
			m.Platforms[0].ArchiveDigest = strings.Repeat("0", 64)
		},
		"binary digest": func(_ *testing.T, m *rc.GatewayManifest, _ *[]byte) {
			m.Platforms[0].BinaryDigest = strings.Repeat("0", 64)
		},
		"other version": func(t *testing.T, m *rc.GatewayManifest, archive *[]byte) {
			bin := gatewayScript("0.1.135")
			*archive = gatewayArchive(t, bin)
			m.Platforms[0].BinaryDigest = sha(bin)
		},
		"traversal": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) {
			*archive = tarGz(t, reg("../sub2api-gateway", 8))
		},
		"nested": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) {
			*archive = tarGz(t, reg("bin/sub2api-gateway", 8))
		},
		"second file": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) {
			*archive = tarGz(t, reg("sub2api-gateway", 8), reg("extra", 1))
		},
		"empty archive": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) { *archive = tarGz(t) },
		"symlink": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) {
			*archive = tarGz(t, &tar.Header{Name: "sub2api-gateway", Linkname: "/bin/sh", Typeflag: tar.TypeSymlink})
		},
		"directory": func(t *testing.T, _ *rc.GatewayManifest, archive *[]byte) {
			*archive = tarGz(t, &tar.Header{Name: "sub2api-gateway", Mode: 0755, Typeflag: tar.TypeDir})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t, "0.1.136", func(m *rc.GatewayManifest, archive *[]byte) {
				mutate(t, m, archive)
				if name != "archive digest" {
					m.Platforms[0].ArchiveDigest, m.Platforms[0].ArchiveBytes = sha(*archive), int64(len(*archive))
				}
			})
			if err := f.install(); err == nil {
				t.Fatal("unsafe gateway archive installed")
			}
			if _, err := os.Stat(filepath.Join(f.m.Root, "gateway", "0.1.136")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a failed install left a version behind: %v", err)
			}
		})
	}
	// The binary's size is bounded before a byte of it is written: the
	// header claims more than MaxGatewayBinaryBytes.
	var huge bytes.Buffer
	gz := gzip.NewWriter(&huge)
	if err := tar.NewWriter(gz).WriteHeader(&tar.Header{Name: "sub2api-gateway", Mode: 0755, Size: MaxGatewayBinaryBytes + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_ = gz.Close()
	var written bytes.Buffer
	if err := CopyGatewayBinary(&huge, &written); err == nil || !strings.Contains(err.Error(), "exceeds") || written.Len() != 0 {
		t.Fatalf("oversized binary: %v (%d bytes written)", err, written.Len())
	}
	// Not executable: pack-gateway would not have packed it either.
	if err := CopyGatewayBinary(bytes.NewReader(tarGz(t, &tar.Header{Name: "sub2api-gateway", Mode: 0644, Size: 1, Typeflag: tar.TypeReg})), io.Discard); err == nil {
		t.Fatal("a non-executable binary was accepted")
	}
	f := newInstallFixture(t, "0.1.136", nil)
	if _, _, err := f.m.InstallGateway(context.Background(), f.manifest[:len(f.manifest)-1], f.open); err == nil || f.opened != 0 {
		t.Fatalf("a malformed manifest was downloaded: %v (opened %d)", err, f.opened)
	}
}

func TestGatewayPointerSwitchRevertPrune(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	install := func(version string) string {
		bin := gatewayScript(version)
		if err := os.MkdirAll(filepath.Dir(m.GatewayBinary(version)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.GatewayBinary(version), bin, 0755); err != nil {
			t.Fatal(err)
		}
		return sha(bin)
	}
	pointer := func() GatewayPointer {
		t.Helper()
		p, err := m.ReadGatewayPointer()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := m.ReadGatewayPointer(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no pointer yet: %v", err)
	}
	d1, d2, d3 := install("1.0.1"), install("1.0.2"), install("1.0.3")
	if err := m.SwitchGateway("1.0.1", strings.Repeat("0", 64), "1.0.0"); err == nil {
		t.Fatal("a binary that differs from its digest was selected")
	}
	if err := m.SwitchGateway("1.0.1", d1, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if p := pointer(); p.Version != "1.0.1" || p.Digest != d1 || p.Previous == nil || !p.Previous.Bundled || p.Previous.Version != "1.0.0" {
		t.Fatalf("first switch: %+v", p)
	}
	if err := m.SwitchGateway("1.0.1", d1, "1.0.1"); err != nil || pointer().Previous.Version != "1.0.0" {
		t.Fatalf("a repeated switch must keep its previous: %v %+v", err, pointer())
	}
	if err := m.SwitchGateway("1.0.2", d2, "1.0.1"); err != nil {
		t.Fatal(err)
	}
	if p := pointer(); p.Previous.Bundled || p.Previous.Version != "1.0.1" || p.Previous.Digest != d1 || p.Previous.Previous != nil {
		t.Fatalf("second switch: %+v", p)
	}
	if err := m.SwitchGateway("1.0.3", d3, "1.0.2"); err != nil {
		t.Fatal(err)
	}
	if err := m.PruneGateways(); err != nil {
		t.Fatal(err)
	}
	for v, kept := range map[string]bool{"1.0.1": false, "1.0.2": true, "1.0.3": true} {
		if _, err := os.Stat(m.GatewayBinary(v)); (err == nil) != kept {
			t.Fatalf("%s kept=%v: %v", v, kept, err)
		}
	}
	if err := m.RevertGateway("1.0.9"); err != nil || pointer().Version != "1.0.3" {
		t.Fatalf("a node never switched to 1.0.9 stays: %v %+v", err, pointer())
	}
	if err := m.RevertGateway("1.0.3"); err != nil || pointer().Version != "1.0.2" || pointer().Previous != nil {
		t.Fatalf("revert: %v %+v", err, pointer())
	}
	if err := m.RevertGateway("1.0.2"); err == nil {
		t.Fatal("a revert without a recorded previous gateway succeeded")
	}
	if err := m.SwitchGateway("1.0.3", d3, "1.0.2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.GatewayBinary("1.0.2"), []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.RevertGateway("1.0.3"); err == nil || pointer().Version != "1.0.3" {
		t.Fatalf("a revert to a damaged binary: %v %+v", err, pointer())
	}
	// Back to the bundled binary.
	m = &Manager{Root: t.TempDir()}
	d1 = install("1.0.1")
	if err := m.SwitchGateway("1.0.1", d1, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := m.RevertGateway("1.0.1"); err != nil {
		t.Fatal(err)
	}
	if p := pointer(); !p.Bundled || p.Version != "1.0.0" {
		t.Fatalf("revert to bundled: %+v", p)
	}
	if err := m.PruneGateways(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.GatewayBinary("1.0.1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a bundled gateway keeps no installed version")
	}
}

func TestHandoffTarget(t *testing.T) {
	installed := GatewayPointer{Version: "1.2.0", Digest: strings.Repeat("a", 64)}
	missing := os.ErrNotExist
	cases := []struct {
		name            string
		p               GatewayPointer
		err             error
		running, marker string
		want            string
		warn            bool
	}{
		{"no pointer", GatewayPointer{}, missing, "1.1.0", "", "", false},
		{"newer installed", installed, nil, "1.1.0", "", "1.2.0", false},
		{"dev build", installed, nil, "0.0.0-dev", "", "1.2.0", false},
		{"this is it", installed, nil, "1.2.0", "1.2.0", "", false},
		{"bundled selected", GatewayPointer{Version: "1.1.0", Bundled: true}, nil, "1.1.0", "", "", false},
		{"older installed", installed, nil, "1.3.0", "", "", true},
		{"misreported version", installed, nil, "1.1.0", "1.2.0", "", true},
		{"corrupt pointer", GatewayPointer{}, errors.New("gateway pointer is corrupt"), "1.1.0", "", "", true},
	}
	for _, c := range cases {
		got, err := HandoffTarget(c.p, c.err, c.running, c.marker)
		if got != c.want || (err != nil) != c.warn || errors.Is(err, ErrStaleGateway) != (c.name == "older installed") {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
}

func TestGatewayPointerRejectsCorruption(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(m.Root, "gateway"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"{", `{"version":"v1"}`, `{"version":"1.0.0"}`, `{"version":"1.0.0","digest":"x"}`, `{"version":"1.0.0","bundled":true,"digest":"` + strings.Repeat("a", 64) + `"}`} {
		if err := os.WriteFile(filepath.Join(m.Root, "gateway", "current"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ReadGatewayPointer(); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s accepted: %v", body, err)
		}
	}
}

func TestNoteHandoffStopsACrashLoop(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if !m.GatewayWritable() {
		t.Fatal("a temporary root is writable")
	}
	for i := 0; i < maxHandoffAttempts; i++ {
		if err := m.NoteHandoff("1.2.0"); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if err := m.NoteHandoff("1.2.0"); err == nil {
		t.Fatal("a gateway that never reached serving was started again")
	}
	if err := m.NoteHandoff("1.3.0"); err != nil {
		t.Fatalf("another version starts its own count: %v", err)
	}
	if err := m.ServingGateway("1.2.0"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxHandoffAttempts; i++ {
		if err := m.NoteHandoff("1.3.0"); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.NoteHandoff("1.3.0"); err == nil {
		t.Fatal("another version serving reset the count")
	}
	if err := m.ServingGateway("1.3.0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxHandoffAttempts; i++ {
		if err := m.NoteHandoff("1.3.0"); err != nil {
			t.Fatalf("serving resets the count: %v", err)
		}
	}
}
