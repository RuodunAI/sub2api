// sub2api-release packages already-built binaries into release manifests and
// bundles (CONTRACTS §33, §102). A manifest is plain JSON; its SHA-256 is the
// release digest.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

const usage = "usage: sub2api-release pubkey|pack|pack-gateway|mirror [flags]"

var commands = map[string]func([]string, io.Writer) error{
	"pubkey":       pubkey,
	"pack":         pack,
	"pack-gateway": packGateway,
	"mirror":       mirror,
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return errors.New("unknown command; " + usage)
	}
	return cmd(args[1:], out)
}

// pubkey prints the base64 public half of a plugin signing key file
// (sub2api-plugin keygen): the content of a core bundle's builtin/trust.pub.
func pubkey(args []string, out io.Writer) error {
	f := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	key := f.String("key", "", "base64 Ed25519 private key or seed file")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *key == "" {
		return errors.New("--key is required")
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	return err
}

func pack(args []string, out io.Writer) error {
	f := flag.NewFlagSet("pack", flag.ContinueOnError)
	dest := f.String("out", "", "output directory")
	dir := f.String("dir", "", "staged release directory (bin/sub2api required) of --os/--arch")
	var platforms platformFlag
	f.Var(&platforms, "platform", "os/arch=DIR: one staged release directory per platform (repeatable)")
	version := f.String("release-id", "", "immutable release version")
	coreVersion := f.String("core-version", "", "actual core binary version")
	commit := f.String("source-commit", "", "source commit or snapshot digest")
	build := f.String("build-id", "", "build identity (defaults to release ID)")
	schema := f.String("schema", "", "target core's embedded database contract")
	schemaBefore := f.String("schema-before", "", "source database contract (defaults to --schema for unchanged schema)")
	osName := f.String("os", runtime.GOOS, "target OS of --dir")
	arch := f.String("arch", runtime.GOARCH, "target architecture of --dir")
	abi := f.String("runtime-abi", "linux-static-v1", "runtime ABI")
	hostAPI := f.Int("host-api", rc.HostAPIVersion, "plugin Host API version")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dir != "" {
		if err := platforms.Set(*osName + "/" + *arch + "=" + *dir); err != nil {
			return err
		}
	}
	if *dest == "" || len(platforms) == 0 || *version == "" || *coreVersion == "" || *commit == "" || *schema == "" {
		return errors.New("--out, --dir or --platform, --release-id, --core-version, --source-commit and --schema are required")
	}
	if *schemaBefore == "" {
		*schemaBefore = *schema
	}
	output, err := filepath.Abs(*dest)
	if err != nil {
		return err
	}
	if *build == "" {
		*build = *version
	}
	if err = os.MkdirAll(output, 0755); err != nil {
		return err
	}
	m := rc.Manifest{ManifestVersion: 1, ReleaseID: *version, CoreVersion: *coreVersion, SourceCommit: *commit, BuildID: *build, CreatedAt: time.Now().UTC(), ShellProtocol: rc.Range{Min: rc.Protocol, Max: rc.Protocol}, CoreControlProtocol: rc.Range{Min: rc.Protocol, Max: rc.Protocol}, ClusterProtocol: rc.Range{Min: 1, Max: 1}, TaskProtocol: rc.Range{Min: 1, Max: 1}, SchemaBefore: *schemaBefore, SchemaAfter: *schema, Strategy: releaseStrategy(*schemaBefore, *schema), HostAPIVersion: *hostAPI}
	for _, p := range platforms {
		platform, err := packPlatform(p.Value, output)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", p.OS, p.Arch, err)
		}
		platform.OS, platform.Arch, platform.RuntimeABI = p.OS, p.Arch, *abi
		m.Platforms = append(m.Platforms, platform)
	}
	manifest, manifestDigest, err := encodeManifest(m)
	if err != nil {
		return err
	}
	if err = writeNew(filepath.Join(output, manifestDigest+".json"), manifest, 0644); err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out, "manifest_digest=%s\n", manifestDigest); err != nil {
		return err
	}
	for _, p := range m.Platforms {
		if _, err = fmt.Fprintf(out, "bundle_digest=%s\n", p.BundleDigest); err != nil {
			return err
		}
	}
	return nil
}

// packPlatform bundles one staged directory into output/<digest>.tar.gz and
// returns its declaration; the caller fills in OS, Arch and RuntimeABI.
func packPlatform(dir, output string) (rc.Platform, error) {
	var p rc.Platform
	root, err := filepath.Abs(dir)
	if err != nil {
		return p, err
	}
	if rel, err := filepath.Rel(root, output); err == nil && (rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")) {
		return p, errors.New("output must be outside the staged release")
	}
	tmp, err := os.CreateTemp(output, ".release-*.tar.gz")
	if err != nil {
		return p, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if p.Files, err = bundle(root, tmp); err != nil {
		return p, err
	}
	if err = tmp.Sync(); err != nil {
		return p, err
	}
	if err = tmp.Close(); err != nil {
		return p, err
	}
	if p.BundleDigest, p.BundleBytes, err = fileDigest(tmp.Name()); err != nil {
		return p, err
	}
	return p, publishFile(tmp.Name(), filepath.Join(output, p.BundleDigest+".tar.gz"))
}

// encodeManifest returns the manifest file of m, indented JSON, and its
// SHA-256: the release digest a gateway computes over the same bytes.
func encodeManifest(m any) ([]byte, string, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, "", err
	}
	raw = append(raw, '\n')
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

// loadKey reads a base64 Ed25519 32-byte seed (sub2api-plugin keygen) or
// 64-byte private key, as sub2api-plugin sign does.
func loadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	switch {
	case err == nil && len(decoded) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	case err == nil && len(decoded) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	}
	return nil, errors.New("invalid Ed25519 private key")
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// platformFlag collects repeatable os/arch=VALUE flags.
type platformFlag []platformValue

type platformValue struct{ OS, Arch, Value string }

var platformName = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (p *platformFlag) String() string { return "" }

func (p *platformFlag) Set(s string) error {
	target, value, ok := strings.Cut(s, "=")
	osName, arch, ok2 := strings.Cut(target, "/")
	if !ok || !ok2 || !platformName.MatchString(osName) || !platformName.MatchString(arch) || value == "" {
		return fmt.Errorf("%q: want os/arch=path", s)
	}
	for _, v := range *p {
		if v.OS == osName && v.Arch == arch {
			return fmt.Errorf("platform %s/%s given twice", osName, arch)
		}
	}
	*p = append(*p, platformValue{OS: osName, Arch: arch, Value: value})
	return nil
}

func bundle(root string, out io.Writer) ([]rc.File, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("stage must be a real directory")
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	var files []rc.File
	var total int64
	core := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink not allowed: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file not allowed: %s", p)
		}
		total += info.Size()
		if total > 4<<30 || len(files) >= 10000 {
			return errors.New("release exceeds size/file limits")
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		mode := uint32(0644)
		if info.Mode().Perm()&0111 != 0 || rel == "bin/sub2api" || rel == "bin/sub2api-plugin" {
			mode = 0755
		}
		if rel == "bin/sub2api" {
			core = true
		}
		if err = tw.WriteHeader(&tar.Header{Name: rel, Mode: int64(mode), Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, info.Size()+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != info.Size() {
			return errors.New("staged file changed while packing")
		}
		files = append(files, rc.File{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Mode: mode})
		return nil
	})
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return nil, err
	}
	if !core {
		return nil, errors.New("missing bin/sub2api")
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	return files, nil
}

func writeNew(path string, b []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
func publishFile(src, dst string) error {
	// Hard-link publication is atomic and cannot overwrite an existing release.
	err := os.Link(src, dst)
	if errors.Is(err, os.ErrExist) {
		a, ae := os.ReadFile(src)
		b, be := os.ReadFile(dst)
		if ae == nil && be == nil && sha256.Sum256(a) == sha256.Sum256(b) {
			return nil
		}
	}
	return err
}

// releaseStrategy declares a release that keeps the schema "rolling": the
// shells may then restart one node at a time while the others serve
// (control.Rolling). A release that migrates is "maintenance" and upgrades
// primary-first.
func releaseStrategy(schemaBefore, schemaAfter string) string {
	if schemaBefore != "" && schemaBefore == schemaAfter {
		return "rolling"
	}
	return "maintenance"
}
