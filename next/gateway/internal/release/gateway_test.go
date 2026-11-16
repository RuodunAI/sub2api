package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

func TestTrustedKeysMergeBuiltinAndConfigured(t *testing.T) {
	builtinKeys, err := TrustedKeys(nil)
	if err != nil || len(builtinKeys) != 0 {
		t.Fatalf("built-in keys: %v %v", builtinKeys, err)
	}
	a, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _ := ed25519.GenerateKey(rand.Reader)
	enc := func(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }
	files := fstest.MapFS{
		"trusted/README.md":    {Data: []byte("docs")},
		"trusted/official.pub": {Data: []byte(enc(a) + "\n")},
	}
	keys, err := mergeKeys(files, map[string]string{"official": enc(a), "fork": enc(b)})
	if err != nil || !keys["official"].Equal(a) || !keys["fork"].Equal(b) || len(keys) != 2 {
		t.Fatalf("merged: %v %v", keys, err)
	}
	if _, err = mergeKeys(files, map[string]string{"official": enc(b)}); err == nil {
		t.Fatal("configured key replaced a built-in key of the same id")
	}
	if _, err = mergeKeys(files, map[string]string{"bad": "AAAA"}); err == nil {
		t.Fatal("malformed configured key accepted")
	}
	files["trusted/broken.pub"] = &fstest.MapFile{Data: []byte("not base64")}
	if _, err = mergeKeys(files, nil); err == nil {
		t.Fatal("malformed built-in key accepted")
	}
}

func gatewayFixture(t *testing.T) (rc.GatewayManifest, ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := rc.GatewayManifest{Kind: rc.GatewayManifestKind, GatewayVersion: "0.1.136", ReleaseID: "gw", BuildID: "gw", PeerProtocol: 2, RuntimeProtocol: rc.Protocol,
		Platforms: []rc.GatewayPlatform{{OS: "linux", Arch: "amd64", Archive: "sub2api-gateway_0.1.136_linux_amd64.tar.gz", ArchiveDigest: strings.Repeat("a", 64), ArchiveBytes: 10, BinaryDigest: strings.Repeat("b", 64)}}}
	return m, priv, map[string]ed25519.PublicKey{"k": pub}
}

func signPayload(priv ed25519.PrivateKey, payload []byte) rc.SignedManifest {
	return rc.SignedManifest{KeyID: "k", Payload: payload, Signature: ed25519.Sign(priv, payload)}
}

func TestVerifyGateway(t *testing.T) {
	m, priv, keys := gatewayFixture(t)
	payload, _ := json.Marshal(m)
	got, digest, err := VerifyGateway(signPayload(priv, payload), keys)
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
			bad, _, _ := gatewayFixture(t)
			mutate(&bad)
			payload, _ := json.Marshal(bad)
			if _, _, err := VerifyGateway(signPayload(priv, payload), keys); err == nil {
				t.Fatal("invalid gateway manifest accepted")
			}
		})
	}
	// A field a later release adds is ignored; data after the document is not.
	extra := append(payload[:len(payload)-1:len(payload)-1], []byte(`,"extra":true}`)...)
	if _, _, err := VerifyGateway(signPayload(priv, extra), keys); err != nil {
		t.Fatalf("payload with a new optional field: %v", err)
	}
	trailing := append(append([]byte{}, payload...), []byte(` {}`)...)
	if _, _, err := VerifyGateway(signPayload(priv, trailing), keys); err == nil {
		t.Fatal("payload with trailing data accepted")
	}
	tampered := signPayload(priv, payload)
	tampered.Signature[0] ^= 1
	if _, _, err := VerifyGateway(tampered, keys); err == nil {
		t.Fatal("bad signature accepted")
	}
	if _, _, err := VerifyGateway(signPayload(priv, payload), map[string]ed25519.PublicKey{}); err == nil || !strings.Contains(err.Error(), `untrusted key "k"`) {
		t.Fatalf("untrusted key: %v", err)
	}
	// A core manifest under the same key is no gateway release, nor the reverse.
	core, _, _ := fixture(t)
	if _, _, err := VerifyGateway(signPayload(priv, core.Payload), keys); err == nil {
		t.Fatal("core manifest verified as a gateway release")
	}
	if _, _, err := Verify(signPayload(priv, payload), keys); err == nil {
		t.Fatal("gateway manifest verified as a core release")
	}
}

func TestPrepareFromVerifiesTheOpenedBundle(t *testing.T) {
	s, bundle, keys := fixture(t)
	opener := func(body []byte, err error) Opener {
		return func(context.Context) (io.ReadCloser, error) {
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	for _, body := range [][]byte{bundle[:len(bundle)-1], bytes.Repeat([]byte("x"), len(bundle))} {
		m := &Manager{Root: t.TempDir(), TrustedKeys: keys, OS: "linux", Arch: "amd64", RuntimeABI: "linux-static-v1"}
		if _, err := m.PrepareFrom(context.Background(), s, opener(body, nil)); err == nil {
			t.Fatal("corrupt bundle installed")
		}
	}
	m := &Manager{Root: t.TempDir(), TrustedKeys: keys, OS: "linux", Arch: "amd64", RuntimeABI: "linux-static-v1"}
	if _, err := m.PrepareFrom(context.Background(), s, opener(nil, errors.New("offline"))); err == nil {
		t.Fatal("failed opener reported success")
	}
	if _, err := m.PrepareFrom(context.Background(), s, opener(bundle, nil)); err != nil {
		t.Fatal(err)
	}
}
