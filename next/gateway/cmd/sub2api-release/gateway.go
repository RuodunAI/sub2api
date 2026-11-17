package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/control"
	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// gatewayManifestAsset is the release asset holding the gateway manifest.
const gatewayManifestAsset = "next-gateway-manifest.json"

var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// packGateway writes the manifest of a gateway release: one archive per
// platform plus the container images built from the same source.
func packGateway(args []string, out io.Writer) error {
	f := flag.NewFlagSet("pack-gateway", flag.ContinueOnError)
	dest := f.String("out", "", "output directory for "+gatewayManifestAsset)
	version := f.String("version", "", "gateway version (semver without prefix)")
	releaseID := f.String("release-id", "", "immutable release ID (defaults to gateway-v<version>)")
	commit := f.String("source-commit", "", "source commit")
	build := f.String("build-id", "", "build identity (defaults to release ID)")
	var archives platformFlag
	f.Var(&archives, "archive", "os/arch=FILE: sub2api-gateway_<version>_<os>_<arch>.tar.gz (repeatable)")
	var images []string
	f.Func("image", "REF@sha256:DIGEST of a pushed image (repeatable)", func(s string) error {
		images = append(images, s)
		return nil
	})
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dest == "" || *version == "" || *commit == "" || len(archives) == 0 {
		return errors.New("--out, --version, --source-commit and --archive are required")
	}
	if v, err := semver.StrictNewVersion(*version); err != nil || v.Prerelease() != "" || v.Metadata() != "" {
		return errors.New("--version must be a stable semantic version without prefix")
	}
	if *releaseID == "" {
		*releaseID = "gateway-v" + *version
	}
	if *build == "" {
		*build = *releaseID
	}
	m := rc.GatewayManifest{Kind: rc.GatewayManifestKind, GatewayVersion: *version, ReleaseID: *releaseID, SourceCommit: *commit, BuildID: *build, CreatedAt: time.Now().UTC(), PeerProtocol: control.PeerProtocol, RuntimeProtocol: rc.Protocol}
	for _, a := range archives {
		p, err := gatewayPlatform(a, *version)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Value, err)
		}
		m.Platforms = append(m.Platforms, p)
	}
	for _, s := range images {
		ref, digest, ok := strings.Cut(s, "@")
		if !ok || ref == "" || !imageDigest.MatchString(digest) {
			return fmt.Errorf("--image %q: want REF@sha256:DIGEST", s)
		}
		m.Images = append(m.Images, rc.GatewayImage{Ref: ref, Digest: digest})
	}
	manifest, digest, err := encodeManifest(m)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(*dest, 0755); err != nil {
		return err
	}
	if err = writeNew(filepath.Join(*dest, gatewayManifestAsset), manifest, 0644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "manifest_digest=%s\n", digest)
	return err
}

// gatewayPlatform declares one archive after checking it as an installing
// gateway will (release.CopyGatewayBinary).
func gatewayPlatform(a platformValue, version string) (rc.GatewayPlatform, error) {
	p := rc.GatewayPlatform{OS: a.OS, Arch: a.Arch, Archive: fmt.Sprintf("sub2api-gateway_%s_%s_%s.tar.gz", version, a.OS, a.Arch)}
	if filepath.Base(a.Value) != p.Archive {
		return p, fmt.Errorf("the %s/%s archive must be named %s", a.OS, a.Arch, p.Archive)
	}
	var err error
	if p.ArchiveDigest, p.ArchiveBytes, err = fileDigest(a.Value); err != nil {
		return p, err
	}
	f, err := os.Open(a.Value)
	if err != nil {
		return p, err
	}
	defer f.Close()
	sum := sha256.New()
	if err = release.CopyGatewayBinary(f, sum); err != nil {
		return p, err
	}
	p.BinaryDigest = hex.EncodeToString(sum.Sum(nil))
	return p, nil
}
