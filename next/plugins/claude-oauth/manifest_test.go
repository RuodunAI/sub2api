package main

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

//go:embed manifest.json
var testManifest []byte

func TestManifestValid(t *testing.T) {
	var m manifest.Manifest
	if err := json.Unmarshal(testManifest, &m); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	if m.APIVersion != 1 {
		t.Errorf("apiVersion = %d, want 1", m.APIVersion)
	}
	if m.Key != "claude_oauth" {
		t.Errorf("key = %q, want claude_oauth", m.Key)
	}
	if m.Runtime != "grpc" {
		t.Errorf("runtime = %q, want grpc", m.Runtime)
	}
}

// TestNoClientHeaders: both account types are handed no client header. The
// released plugin sent the upstream no client header, and forwarding v2
// keeps what it sends; "passHeaders": [] (not absent, which would mean the
// anthropic platform's list) tells the core to hand over none.
func TestNoClientHeaders(t *testing.T) {
	var m manifest.Manifest
	if err := json.Unmarshal(testManifest, &m); err != nil {
		t.Fatal(err)
	}
	for _, at := range m.AccountTypes {
		for _, ap := range at.Platforms {
			if ap.PassHeaders == nil || len(ap.ClientHeaders(&manifest.Platform{PassHeaders: []string{"user-agent"}})) != 0 {
				t.Errorf("%s/%s: passHeaders %v, want an explicit empty list", at.ID, ap.Platform, ap.PassHeaders)
			}
		}
	}
}
