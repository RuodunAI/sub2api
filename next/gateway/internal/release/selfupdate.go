package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// The installed gateways live under <root>/gateway: one directory per
// version holding the binary, and the pointer file current, which names the
// version the gateway starts (one-click gateway updates, CONTRACTS §102.9).
const (
	gatewayDir    = "gateway"
	gatewayBinary = "sub2api-gateway"
	// MaxGatewayBinaryBytes bounds the binary a gateway archive holds.
	MaxGatewayBinaryBytes = 512 << 20
	// maxHandoffAttempts is how often a startup hands off to an installed
	// gateway that never reaches serving before it stays on the bundled one.
	maxHandoffAttempts = 3
	probeTimeout       = 10 * time.Second
)

// GatewayPointer is <root>/gateway/current: the installed version the
// gateway starts, with the SHA-256 of its binary, or Bundled for the image's
// own binary. Previous is what ran before the last swap, for a rollback.
type GatewayPointer struct {
	Version  string          `json:"version"`
	Digest   string          `json:"digest,omitempty"`
	Bundled  bool            `json:"bundled,omitempty"`
	Previous *GatewayPointer `json:"previous,omitempty"`
}

// GatewayBinary is the installed binary of version.
func (m *Manager) GatewayBinary(version string) string {
	return filepath.Join(m.Root, gatewayDir, version, gatewayBinary)
}

// GatewayWritable reports whether this node can install gateways: it creates
// <root>/gateway and a file in it.
func (m *Manager) GatewayWritable() bool {
	dir := filepath.Join(m.Root, gatewayDir)
	if os.MkdirAll(dir, 0700) != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-")
	if err != nil {
		return false
	}
	f.Close()
	return os.Remove(f.Name()) == nil
}

// GatewayPlatform is the archive of m for this node's platform.
func (m *Manager) GatewayPlatform(v rc.GatewayManifest) (rc.GatewayPlatform, error) {
	for _, p := range v.Platforms {
		if p.OS == m.OS && p.Arch == m.Arch {
			return p, nil
		}
	}
	return rc.GatewayPlatform{}, fmt.Errorf("the gateway release has no archive for %s/%s", m.OS, m.Arch)
}

// ArchiveOpener opens a gateway archive by its asset name and signed size.
type ArchiveOpener func(ctx context.Context, archive string, size int64) (io.ReadCloser, error)

// InstallGateway verifies the signed gateway release s, downloads this
// platform's archive through open, checks its size and digest, extracts its
// single regular file sub2api-gateway, checks the binary's digest and that it
// reports the signed version, and installs it as <root>/gateway/<version>
// with one rename. An intact installation is kept. It returns the payload
// digest and the manifest.
func (m *Manager) InstallGateway(ctx context.Context, s rc.SignedManifest, open ArchiveOpener) (string, rc.GatewayManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, digest, err := VerifyGateway(s, m.TrustedKeys)
	if err != nil {
		return "", v, err
	}
	p, err := m.GatewayPlatform(v)
	if err != nil {
		return digest, v, err
	}
	dest := filepath.Join(m.Root, gatewayDir, v.GatewayVersion)
	if verifyBinary(filepath.Join(dest, gatewayBinary), p.BinaryDigest) == nil {
		return digest, v, nil
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return digest, v, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".stage-")
	if err != nil {
		return digest, v, err
	}
	defer os.RemoveAll(stage)
	archive := filepath.Join(stage, "archive.tar.gz")
	err = m.fetch(ctx, func(ctx context.Context) (io.ReadCloser, error) { return open(ctx, p.Archive, p.ArchiveBytes) }, archive, p.ArchiveBytes, p.ArchiveDigest)
	if err != nil {
		return digest, v, err
	}
	dir := filepath.Join(stage, "version")
	if err = os.Mkdir(dir, 0700); err != nil {
		return digest, v, err
	}
	bin := filepath.Join(dir, gatewayBinary)
	if err = extractGateway(ctx, archive, bin); err != nil {
		return digest, v, err
	}
	if err = verifyBinary(bin, p.BinaryDigest); err != nil {
		return digest, v, err
	}
	if err = probeVersion(ctx, bin, v.GatewayVersion); err != nil {
		return digest, v, err
	}
	if err = syncDir(dir); err != nil {
		return digest, v, err
	}
	// A damaged installation of the version makes room for the new one.
	if err = os.RemoveAll(dest); err != nil {
		return digest, v, err
	}
	if err = os.Rename(dir, dest); err != nil {
		return digest, v, err
	}
	return digest, v, syncDir(filepath.Dir(dest))
}

// errOneBinary rejects a gateway archive that holds anything but its binary.
var errOneBinary = errors.New("gateway archive must hold only the executable regular file " + gatewayBinary)

// CopyGatewayBinary copies the binary of a gateway archive (tar.gz) to dst.
// The archive must hold one entry, the executable regular file
// sub2api-gateway of at most MaxGatewayBinaryBytes. pack-gateway applies the
// same check, so a release it signs installs.
func CopyGatewayBinary(archive io.Reader, dst io.Writer) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return fmt.Errorf("gateway archive is unreadable: %w", err)
	}
	if hdr.Typeflag != tar.TypeReg || hdr.Name != gatewayBinary || hdr.Mode&0111 == 0 {
		return errOneBinary
	}
	if hdr.Size <= 0 || hdr.Size > MaxGatewayBinaryBytes {
		return fmt.Errorf("gateway binary exceeds %d bytes", MaxGatewayBinaryBytes)
	}
	if _, err = io.Copy(dst, tr); err != nil {
		return err
	}
	if _, err = tr.Next(); err != io.EOF {
		return errOneBinary
	}
	return nil
}

// extractGateway writes the binary of the archive file to dst, mode 0755.
func extractGateway(ctx context.Context, archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	err = CopyGatewayBinary(f, out)
	if err == nil {
		err = out.Chmod(0755)
	}
	if err == nil {
		err = out.Sync()
	}
	if ce := out.Close(); err == nil {
		err = ce
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// verifyBinary checks that path is a regular 0755 file with the digest.
func verifyBinary(path, digest string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0755 {
		return errors.New("installed gateway binary is not a regular 0755 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("installed gateway binary differs from its signed digest")
	}
	return nil
}

// probeVersion runs "bin version", which must print version: a binary that
// cannot run here, or that was built as another version, is refused before
// any core stops for it.
func probeVersion(ctx context.Context, bin, version string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the gateway binary does not run on this node: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return fmt.Errorf("the gateway binary reports version %.64q, signed %s", got, version)
	}
	return nil
}

// ReadGatewayPointer reads current; os.ErrNotExist means none was written.
func (m *Manager) ReadGatewayPointer() (GatewayPointer, error) {
	var p GatewayPointer
	b, err := os.ReadFile(filepath.Join(m.Root, gatewayDir, "current"))
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(b, &p); err != nil || !validPointer(p) {
		return p, errors.New("gateway pointer is corrupt")
	}
	return p, nil
}

func validPointer(p GatewayPointer) bool {
	if _, err := semver.StrictNewVersion(p.Version); err != nil {
		return false
	}
	return p.Bundled == (p.Digest == "") && (p.Bundled || ValidDigest(p.Digest))
}

// SwitchGateway points current at the installed version, whose binary must
// still have its signed digest, and records what runs now (running, from
// the pointer when it names it, else the bundled binary) as previous.
func (m *Manager) SwitchGateway(version, digest, running string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := verifyBinary(m.GatewayBinary(version), digest); err != nil {
		return err
	}
	cur, err := m.ReadGatewayPointer()
	if err == nil && !cur.Bundled && cur.Version == version {
		return nil
	}
	prev := GatewayPointer{Version: running, Bundled: true}
	if err == nil && !cur.Bundled && cur.Version == running {
		prev = GatewayPointer{Version: cur.Version, Digest: cur.Digest}
	}
	return m.writePointer(GatewayPointer{Version: version, Digest: digest, Previous: &prev})
}

// RevertGateway undoes a switch to version: current goes back to what ran
// before it. A node whose current does not name version is left alone.
func (m *Manager) RevertGateway(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.ReadGatewayPointer()
	if errors.Is(err, os.ErrNotExist) || (err == nil && (cur.Bundled || cur.Version != version)) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Previous == nil {
		return errors.New("gateway pointer records no previous gateway")
	}
	prev := *cur.Previous
	if !prev.Bundled {
		if err = verifyBinary(m.GatewayBinary(prev.Version), prev.Digest); err != nil {
			return fmt.Errorf("previous gateway %s: %w", prev.Version, err)
		}
	}
	return m.writePointer(prev)
}

// writePointer replaces current atomically and starts a new handoff count.
func (m *Manager) writePointer(p GatewayPointer) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(m.Root, gatewayDir, "current"), b); err != nil {
		return err
	}
	return m.clearHandoff()
}

// PruneGateways removes every installed version but the ones current and
// its previous name, and staging left by an interrupted install.
func (m *Manager) PruneGateways() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.ReadGatewayPointer()
	if err != nil {
		return err
	}
	keep := map[string]bool{cur.Version: !cur.Bundled}
	if cur.Previous != nil && !cur.Previous.Bundled {
		keep[cur.Previous.Version] = true
	}
	dir := filepath.Join(m.Root, gatewayDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_, semverErr := semver.StrictNewVersion(e.Name())
		if !e.IsDir() || keep[e.Name()] || (semverErr != nil && !strings.HasPrefix(e.Name(), ".stage-")) {
			continue
		}
		if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// ErrStaleGateway is why a binary newer than the installed gateway keeps
// running: the image moved past the installed version. It is expected after
// an image upgrade and lasts until the next swap.
var ErrStaleGateway = errors.New("the installed gateway is not newer than this binary")

// HandoffTarget decides at startup whether a gateway running version should
// replace itself with the installed one p names (read with err): only a
// newer installed version, and never the one marker says this process was
// started as, so a binary that reports another version cannot exec itself
// in a loop. It returns the version to start, or "" with the reason to keep
// running (ErrStaleGateway for an older installed version).
func HandoffTarget(p GatewayPointer, err error, running, marker string) (string, error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case p.Bundled || p.Version == running:
		return "", nil
	case marker == p.Version:
		return "", fmt.Errorf("gateway %s was started for version %s but reports %s", p.Version, marker, running)
	}
	v, _ := semver.StrictNewVersion(p.Version)
	if r, perr := semver.NewVersion(running); perr == nil && !v.GreaterThan(r) {
		return "", fmt.Errorf("%w: installed %s, running %s", ErrStaleGateway, p.Version, running)
	}
	return p.Version, nil
}

// handoffState counts the startups that handed off to an installed version
// which has not reached serving since.
type handoffState struct {
	Version  string `json:"version"`
	Attempts int    `json:"attempts"`
}

func (m *Manager) handoffFile() string { return filepath.Join(m.Root, gatewayDir, "handoff") }

// NoteHandoff counts a startup handing off to version and fails once the
// installed gateway has failed to reach serving maxHandoffAttempts times:
// the bundled gateway then serves instead of looping on a crashing one.
func (m *Manager) NoteHandoff(version string) error {
	var st handoffState
	if b, err := os.ReadFile(m.handoffFile()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Version != version {
		st = handoffState{Version: version}
	}
	if st.Attempts >= maxHandoffAttempts {
		return fmt.Errorf("installed gateway %s did not reach serving after %d starts", version, st.Attempts)
	}
	st.Attempts++
	b, _ := json.Marshal(st)
	return atomicWrite(m.handoffFile(), b)
}

// ServingGateway records that gateway version reached serving. A count of
// another version stays: the bundled gateway serving in its place must not
// let the next startup try the crashing one again.
func (m *Manager) ServingGateway(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st handoffState
	if b, err := os.ReadFile(m.handoffFile()); err != nil || json.Unmarshal(b, &st) != nil || st.Version != version {
		return nil
	}
	return m.clearHandoff()
}

func (m *Manager) clearHandoff() error {
	if err := os.Remove(m.handoffFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
