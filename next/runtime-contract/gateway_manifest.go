package runtimecontract

import "time"

// GatewayManifestKind separates gateway release manifests from core release
// manifests: a verifier of one kind rejects a document of the other.
const GatewayManifestKind = "sub2api-gateway-release-v1"

// GatewayManifest describes one release of the node gateway
// (sub2api-gateway). Like Manifest, its digest is the SHA-256 of the exact
// published bytes.
type GatewayManifest struct {
	Kind           string    `json:"kind"`
	GatewayVersion string    `json:"gateway_version"`
	ReleaseID      string    `json:"release_id"`
	SourceCommit   string    `json:"source_commit"`
	BuildID        string    `json:"build_id"`
	CreatedAt      time.Time `json:"created_at"`
	// PeerProtocol is the gateway-to-gateway protocol of the build; every
	// gateway of a cluster must agree on it.
	PeerProtocol int `json:"peer_protocol"`
	// RuntimeProtocol is the gateway-to-core Protocol the build speaks; the
	// running core release must list it in Manifest.ShellProtocol.
	RuntimeProtocol int               `json:"runtime_protocol"`
	Platforms       []GatewayPlatform `json:"platforms"`
	// Images are informational container references pinned by digest.
	Images []GatewayImage `json:"images,omitempty"`
}

// GatewayPlatform is one downloadable gateway archive: a tar.gz holding a
// single regular file named sub2api-gateway.
type GatewayPlatform struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Archive       string `json:"archive"`
	ArchiveDigest string `json:"archive_digest"`
	ArchiveBytes  int64  `json:"archive_bytes"`
	BinaryDigest  string `json:"binary_digest"`
}

type GatewayImage struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}
