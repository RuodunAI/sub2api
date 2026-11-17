package pluginsdktest

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// SetSettings sets the values GetSettings answers for the section of v. A
// section never set answers UNAVAILABLE, as a host whose database is down
// would. Pair a change with Harness.SettingsChanged, as the host does.
func (f *FakeHost) SetSettings(v protocol.SettingsValues) {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settings == nil {
		f.settings = map[string]string{}
	}
	f.settings[v.SettingsSection()] = string(raw)
}

// SettingsReads counts the sections GetSettings answered.
func (f *FakeHost) SettingsReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settingsReads
}

// GetSettings answers with the sections set by SetSettings; the version is
// the values themselves, which changes exactly when they do.
func (f *FakeHost) GetSettings(_ context.Context, in *pluginv1.GetSettingsRequest) (*pluginv1.GetSettingsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := in.GetSections()
	if len(names) == 0 {
		names = protocol.SettingsSections
	}
	out := &pluginv1.GetSettingsResponse{}
	for _, name := range names {
		raw, ok := f.settings[name]
		if !ok {
			return nil, status.Errorf(codes.Unavailable, "no %q settings: call FakeHost.SetSettings", name)
		}
		out.Sections = append(out.Sections, &pluginv1.SettingsSection{Name: name, Version: raw, ValuesJson: raw})
	}
	f.settingsReads += len(out.Sections)
	return out, nil
}

// SettingsChanged tells the plugin that sections changed
// (PluginService.SettingsChanged), as the host does on every node.
func (h *Harness) SettingsChanged(sections ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.Plugin.SettingsChanged(ctx, &pluginv1.SettingsChangedRequest{Sections: sections})
	return err
}
