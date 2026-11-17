package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

func gatewayFixture() rc.GatewayManifest {
	return rc.GatewayManifest{Kind: rc.GatewayManifestKind, GatewayVersion: "0.1.136", ReleaseID: "gw", BuildID: "gw", PeerProtocol: 2, RuntimeProtocol: rc.Protocol,
		Platforms: []rc.GatewayPlatform{{OS: "linux", Arch: "amd64", Archive: "sub2api-gateway_0.1.136_linux_amd64.tar.gz", ArchiveDigest: strings.Repeat("a", 64), ArchiveBytes: 10, BinaryDigest: strings.Repeat("b", 64)}}}
}

func TestVerifyGateway(t *testing.T) {
	payload, _ := json.Marshal(gatewayFixture())
	got, digest, err := VerifyGateway(payload)
	if err != nil || got.GatewayVersion != "0.1.136" || digest != Digest(payload) {
		t.Fatalf("valid manifest: %+v %v", got, err)
	}
	for name, mutate := range map[string]func(*rc.GatewayManifest){
		"kind":        func(m *rc.GatewayManifest) { m.Kind = "" },
		"version":     func(m *rc.GatewayManifest) { m.GatewayVersion = "v0.1.136" },
		"platforms":   func(m *rc.GatewayManifest) { m.Platforms = nil },
		"archive":     func(m *rc.GatewayManifest) { m.Platforms[0].Archive = "../x.tar.gz" },
		"digest":      func(m *rc.GatewayManifest) { m.Platforms[0].ArchiveDigest = "x" },
		"size":        func(m *rc.GatewayManifest) { m.Platforms[0].ArchiveBytes = 0 },
		"duplicate":   func(m *rc.GatewayManifest) { m.Platforms = append(m.Platforms, m.Platforms[0]) },
		"release id":  func(m *rc.GatewayManifest) { m.ReleaseID = "" },
		"binary hash": func(m *rc.GatewayManifest) { m.Platforms[0].BinaryDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := gatewayFixture()
			mutate(&bad)
			payload, _ := json.Marshal(bad)
			if _, _, err := VerifyGateway(payload); err == nil {
				t.Fatal("invalid gateway manifest accepted")
			}
		})
	}
	// A field a later release adds is ignored; data after the document is not.
	extra := append(payload[:len(payload)-1:len(payload)-1], []byte(`,"extra":true}`)...)
	if _, _, err := VerifyGateway(extra); err != nil {
		t.Fatalf("payload with a new optional field: %v", err)
	}
	trailing := append(append([]byte{}, payload...), []byte(` {}`)...)
	if _, _, err := VerifyGateway(trailing); err == nil {
		t.Fatal("payload with trailing data accepted")
	}
	// A core manifest is no gateway release, nor the reverse.
	core, _ := fixture(t)
	if _, _, err := VerifyGateway(core); err == nil {
		t.Fatal("core manifest verified as a gateway release")
	}
	if _, _, err := Verify(payload); err == nil {
		t.Fatal("gateway manifest verified as a core release")
	}
}

func TestPrepareFromVerifiesTheOpenedBundle(t *testing.T) {
	s, bundle := fixture(t)
	opener := func(body []byte, err error) Opener {
		return func(context.Context) (io.ReadCloser, error) {
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	for _, body := range [][]byte{bundle[:len(bundle)-1], bytes.Repeat([]byte("x"), len(bundle))} {
		m := &Manager{Root: t.TempDir(), OS: "linux", Arch: "amd64", RuntimeABI: "linux-static-v1"}
		if _, err := m.PrepareFrom(context.Background(), s, opener(body, nil)); err == nil {
			t.Fatal("corrupt bundle installed")
		}
	}
	m := &Manager{Root: t.TempDir(), OS: "linux", Arch: "amd64", RuntimeABI: "linux-static-v1"}
	if _, err := m.PrepareFrom(context.Background(), s, opener(nil, errors.New("offline"))); err == nil {
		t.Fatal("failed opener reported success")
	}
	if _, err := m.PrepareFrom(context.Background(), s, opener(bundle, nil)); err != nil {
		t.Fatal(err)
	}
}
