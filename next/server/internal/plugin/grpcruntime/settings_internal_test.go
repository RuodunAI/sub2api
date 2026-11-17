package grpcruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry"
)

func TestHostGetSettings(t *testing.T) {
	ctx := context.Background()
	i, _ := testInstance(t, nil, nil, registry.Grants{})
	h := &hostServer{i: i}
	if _, err := h.GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{protocol.SettingsSticky}}); status.Code(err) != codes.Unavailable {
		t.Fatalf("no readers: %v", err)
	}
	i.rt.o.Settings = map[string]core.SettingsReader{}
	for _, name := range protocol.SettingsSections {
		i.rt.o.Settings[name] = func(context.Context) (protocol.SettingsValues, error) {
			return protocol.StickySettings{Enabled: name == protocol.SettingsSticky}, nil
		}
	}
	if _, err := h.GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{protocol.SettingsSticky, "secrets"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown section: %v", err)
	}

	// No grant needed; asked twice, a section is answered once.
	r, err := h.GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{protocol.SettingsSticky, protocol.SettingsBilling, protocol.SettingsSticky}})
	if err != nil || len(r.GetSections()) != 2 || r.GetSections()[0].GetName() != protocol.SettingsSticky || r.GetSections()[1].GetName() != protocol.SettingsBilling {
		t.Fatalf("answer %+v %v", r, err)
	}
	s := r.GetSections()[0]
	sum := sha256.Sum256([]byte(s.GetValuesJson()))
	if s.GetValuesJson() != `{"enabled":true,"default_ttl_seconds":0,"keep_on_account_disabled":false}` || s.GetVersion() != hex.EncodeToString(sum[:8]) {
		t.Fatalf("section %+v", s)
	}
	if r.GetSections()[1].GetVersion() == s.GetVersion() {
		t.Fatal("different values share a version")
	}

	all, err := h.GetSettings(ctx, &pluginv1.GetSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range all.GetSections() {
		names = append(names, s.GetName())
	}
	if !slices.Equal(names, protocol.SettingsSections) {
		t.Fatalf("all sections %v", names)
	}

	i.rt.o.Settings[protocol.SettingsSticky] = func(context.Context) (protocol.SettingsValues, error) { return nil, core.ErrUnavailable }
	if _, err := h.GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{protocol.SettingsSticky}}); status.Code(err) != codes.Unavailable {
		t.Fatalf("reader error: %v", err)
	}
}

// settingsPlugin answers SettingsChanged with err and records the sections.
type settingsPlugin struct {
	pluginv1.PluginServiceClient
	err error
	got [][]string
}

func (p *settingsPlugin) SettingsChanged(_ context.Context, in *pluginv1.SettingsChangedRequest, _ ...grpc.CallOption) (*pluginv1.SettingsChangedResponse, error) {
	p.got = append(p.got, in.GetSections())
	return &pluginv1.SettingsChangedResponse{}, p.err
}

// A plugin built before SettingsChanged existed answers UNIMPLEMENTED, which
// is not a failure; any other error is.
func TestInstanceSettingsChanged(t *testing.T) {
	ctx := context.Background()
	i, _ := testInstance(t, nil, nil, registry.Grants{})
	sp := &settingsPlugin{}
	p := *i.proc.Load()
	p.plugin = sp
	i.proc.Store(&p)
	if err := i.SettingsChanged(ctx, []string{protocol.SettingsSticky}); err != nil || len(sp.got) != 1 || sp.got[0][0] != protocol.SettingsSticky {
		t.Fatalf("delivered %v %v", sp.got, err)
	}
	sp.err = status.Error(codes.Unimplemented, "old sdk")
	if err := i.SettingsChanged(ctx, nil); err != nil {
		t.Fatalf("unimplemented: %v", err)
	}
	sp.err = status.Error(codes.Internal, "boom")
	if err := i.SettingsChanged(ctx, nil); err == nil {
		t.Fatalf("failure: %v", err)
	}
}
