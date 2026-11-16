package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
)

type execCall struct {
	path, digest string
	env          []string
}

// recordExec is an execFunc that fails like a refused exec, recording what
// it was asked to run.
func recordExec(calls *[]execCall) execFunc {
	return func(path, digest string, _, env []string) error {
		*calls = append(*calls, execCall{path, digest, env})
		return errors.New("exec refused")
	}
}

func TestStartupHandoffExecsTheNewerInstalledGatewayOnly(t *testing.T) {
	t.Setenv(handoffEnv, "")
	t.Setenv(bundledEnv, "/image/sub2api-gateway")
	t.Setenv(bundledVersionEnv, "")
	root := t.TempDir()
	var calls []execCall
	startupHandoff(root, recordExec(&calls))
	if len(calls) != 0 {
		t.Fatalf("no pointer: %v", calls)
	}

	m := &release.Manager{Root: root}
	bin := m.GatewayBinary("0.1.136")
	body := []byte("installed gateway")
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, body, 0755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	digest := hex.EncodeToString(h[:])
	if err := m.SwitchGateway("0.1.136", digest, Version); err != nil {
		t.Fatal(err)
	}
	startupHandoff(root, recordExec(&calls))
	if len(calls) != 1 || calls[0].path != bin || calls[0].digest != digest {
		t.Fatalf("handoff: %+v", calls)
	}
	for _, kv := range []string{handoffEnv + "=0.1.136", bundledEnv + "=/image/sub2api-gateway", bundledVersionEnv + "=" + Version} {
		if !slices.Contains(calls[0].env, kv) {
			t.Fatalf("handoff env lacks %s", kv)
		}
	}

	// The gateway started for 0.1.136 that reports another version must not
	// exec it again.
	t.Setenv(handoffEnv, "0.1.136")
	startupHandoff(root, recordExec(&calls))
	if len(calls) != 1 {
		t.Fatal("an exec loop was not stopped")
	}

	// An installed gateway that never reaches serving is tried a bounded
	// number of times; the bundled one then serves.
	t.Setenv(handoffEnv, "")
	for i := 0; i < 5; i++ {
		startupHandoff(root, recordExec(&calls))
	}
	if len(calls) != 3 {
		t.Fatalf("starts of a crashing gateway: %d", len(calls))
	}
}

func TestRestartGatewayExecsTheBundledBinaryWithoutTheMarker(t *testing.T) {
	t.Setenv(handoffEnv, "0.1.136")
	t.Setenv(bundledEnv, "/image/sub2api-gateway")
	var calls []execCall
	if err := restartGateway(recordExec(&calls)); err == nil || len(calls) != 1 {
		t.Fatalf("restart: %v %v", err, calls)
	}
	c := calls[0]
	if c.path != "/image/sub2api-gateway" || c.digest != "" || slices.ContainsFunc(c.env, func(kv string) bool { return kv == handoffEnv+"=0.1.136" }) {
		t.Fatalf("restart: %+v", c)
	}
}
