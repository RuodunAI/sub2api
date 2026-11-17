package app

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// Every section of the SDK has exactly one owner.
func TestSettingsReadersOwners(t *testing.T) {
	read := func(context.Context) (protocol.SettingsValues, error) { return nil, nil }
	all := map[string]core.SettingsReader{}
	for _, name := range protocol.SettingsSections {
		all[name] = read
	}
	if got, err := settingsReaders(all); err != nil || len(got) != len(protocol.SettingsSections) {
		t.Fatalf("complete: %v %v", got, err)
	}
	if _, err := settingsReaders(all, map[string]core.SettingsReader{protocol.SettingsSticky: read}); err == nil {
		t.Fatal("two owners accepted")
	}
	if _, err := settingsReaders(map[string]core.SettingsReader{protocol.SettingsSticky: read}); err == nil {
		t.Fatal("missing sections accepted")
	}
	if _, err := settingsReaders(all, map[string]core.SettingsReader{"secrets": read}); err == nil {
		t.Fatal("unknown section accepted")
	}
}
