package grpcruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// System settings for plugins (CONTRACTS §106).

// SettingsChangedTimeout bounds one PluginService.SettingsChanged call.
const SettingsChangedTimeout = 2 * time.Second

// GetSettings needs no grant, like GetEnvironment: the sections are
// operating parameters every plugin is expected to honour, and none holds a
// secret.
func (h *hostServer) GetSettings(ctx context.Context, in *pluginv1.GetSettingsRequest) (*pluginv1.GetSettingsResponse, error) {
	names := in.GetSections()
	if len(names) == 0 {
		names = protocol.SettingsSections
	}
	if i := slices.IndexFunc(names, func(n string) bool { return !slices.Contains(protocol.SettingsSections, n) }); i >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "unknown settings section %q", names[i])
	}
	out := &pluginv1.GetSettingsResponse{}
	for _, name := range names {
		if slices.ContainsFunc(out.Sections, func(s *pluginv1.SettingsSection) bool { return s.GetName() == name }) {
			continue
		}
		read := h.i.rt.o.Settings[name]
		if read == nil {
			return nil, status.Error(codes.Unavailable, "settings unavailable")
		}
		v, err := read(ctx)
		if err != nil {
			return nil, toStatus(err)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, status.Error(codes.Internal, "encode settings")
		}
		sum := sha256.Sum256(raw)
		out.Sections = append(out.Sections, &pluginv1.SettingsSection{Name: name, Version: hex.EncodeToString(sum[:8]), ValuesJson: string(raw)})
	}
	return out, nil
}

// SettingsChanged tells the plugin process that sections changed. A plugin
// built with an SDK that predates the call answers UNIMPLEMENTED: it caches
// nothing, so that is not an error.
func (i *Instance) SettingsChanged(ctx context.Context, sections []string) error {
	return i.call(ctx, classBackground, SettingsChangedTimeout, func(ctx context.Context, p *proc) error {
		_, err := p.plugin.SettingsChanged(ctx, &pluginv1.SettingsChangedRequest{Sections: sections})
		if status.Code(err) == codes.Unimplemented {
			return nil
		}
		return err
	})
}
