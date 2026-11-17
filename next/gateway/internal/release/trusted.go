package release

import (
	"crypto/ed25519"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"strings"
)

// builtin holds the release keys compiled into the gateway: trusted/<id>.pub,
// each the standard base64 of an Ed25519 public key (trusted/README.md).
//
//go:embed trusted
var builtin embed.FS

// TrustedKeys merges the built-in release keys with the configured ones
// (key id -> base64). The same id under two different keys is an error.
func TrustedKeys(configured map[string]string) (map[string]ed25519.PublicKey, error) {
	return mergeKeys(builtin, configured)
}

func mergeKeys(files fs.FS, configured map[string]string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	entries, err := fs.ReadDir(files, "trusted")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".pub")
		if !ok || e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(files, "trusted/"+e.Name())
		if err != nil {
			return nil, err
		}
		if keys[id], err = parseKey(id, string(b)); err != nil {
			return nil, err
		}
	}
	for id, b64 := range configured {
		key, err := parseKey(id, b64)
		if err != nil {
			return nil, err
		}
		if known, ok := keys[id]; ok && !known.Equal(key) {
			return nil, fmt.Errorf("trusted key %q differs from the built-in key of the same id", id)
		}
		keys[id] = key
	}
	return keys, nil
}

func parseKey(id, b64 string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if id == "" || err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid trusted public key %q", id)
	}
	return ed25519.PublicKey(b), nil
}
