package app

import (
	"fmt"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// settingsReaders merges the readers of the plugin-visible settings sections
// (CONTRACTS §107) that their owners hand out. Every section of the SDK has
// exactly one owner, so a new section cannot ship unreadable.
func settingsReaders(owners ...map[string]core.SettingsReader) (map[string]core.SettingsReader, error) {
	out := map[string]core.SettingsReader{}
	for _, owner := range owners {
		for name, read := range owner {
			if out[name] != nil {
				return nil, fmt.Errorf("settings section %q has two owners", name)
			}
			out[name] = read
		}
	}
	for _, name := range protocol.SettingsSections {
		if out[name] == nil {
			return nil, fmt.Errorf("settings section %q has no owner", name)
		}
	}
	if len(out) != len(protocol.SettingsSections) {
		return nil, fmt.Errorf("an owner reads a settings section the SDK does not declare")
	}
	return out, nil
}
