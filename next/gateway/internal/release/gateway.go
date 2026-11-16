package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Masterminds/semver/v3"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// MaxGatewayArchiveBytes bounds one signed gateway archive.
const MaxGatewayArchiveBytes = 1 << 30

// VerifyGateway checks a signed gateway release manifest: the signature under
// keys, one JSON document, the gateway kind, which a core manifest never
// carries, and well-formed platform archives. It returns the manifest and its
// payload digest.
func VerifyGateway(s rc.SignedManifest, keys map[string]ed25519.PublicKey) (rc.GatewayManifest, string, error) {
	var m rc.GatewayManifest
	if err := verifySignature(s, keys); err != nil {
		return m, "", err
	}
	if err := decodeManifest(s.Payload, &m); err != nil {
		return m, "", errors.New("invalid gateway release manifest")
	}
	if m.Kind != rc.GatewayManifestKind {
		return m, "", errors.New("signed manifest is not a gateway release")
	}
	if _, err := semver.StrictNewVersion(m.GatewayVersion); err != nil || m.ReleaseID == "" || m.BuildID == "" || len(m.Platforms) == 0 {
		return m, "", errors.New("unsupported gateway release contract")
	}
	seen := map[string]bool{}
	for _, p := range m.Platforms {
		key := p.OS + "/" + p.Arch
		if p.OS == "" || p.Arch == "" || seen[key] || !ValidFileName(p.Archive) || !ValidDigest(p.ArchiveDigest) || !ValidDigest(p.BinaryDigest) ||
			p.ArchiveBytes <= 0 || p.ArchiveBytes > MaxGatewayArchiveBytes {
			return m, "", errors.New("invalid gateway archive declaration")
		}
		seen[key] = true
	}
	return m, Digest(s.Payload), nil
}

// decodeManifest decodes one JSON document. Unknown fields are ignored, so
// that a release adding an optional field still installs on the gateways
// deployed before it (including the self-update that would teach them the
// field); the kind and contract checks keep the two manifest kinds apart.
func decodeManifest(payload []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing data after the manifest")
	}
	return nil
}

// ValidFileName accepts a plain file name: a release asset at an update
// source or a gateway archive.
func ValidFileName(name string) bool {
	return validPath(name) && len(name) <= 255 && !strings.ContainsAny(name, "/\x00")
}
