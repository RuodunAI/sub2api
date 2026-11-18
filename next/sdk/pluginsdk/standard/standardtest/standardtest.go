// Package standardtest connects a standard.Platform to the frozen golden
// cases (sdk/pluginsdk/standard/goldentest): a plugin built on the standard
// library replays them with
//
//	standardtest.Run(t, "openai", "../../manifest.json", myplugin.New())
package standardtest

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
)

// Run replays every golden case of plugin against p (goldentest.Run with
// Golden). The cases are replayed on the frozen file's passHeaders, so Run
// first requires the plugin's manifest.json (at manifestPath) to hand the
// file's account type exactly those client headers: a manifest that
// changes them fails here instead of passing the replay unnoticed.
func Run(t *testing.T, plugin, manifestPath string, p *standard.Platform, opts ...goldentest.Option) {
	t.Helper()
	f, err := goldentest.Load(plugin)
	if err != nil {
		t.Fatal(err)
	}
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifestPassHeaders(t, rawManifest, f.AccountType); !slices.Equal(got, f.PassHeaders) {
		t.Fatalf("the manifest hands account type %q the client headers %v, the golden cases were recorded with %v", f.AccountType, got, f.PassHeaders)
	}
	goldentest.Run(t, plugin, Golden(p, f.PassHeaders), opts...)
}

// manifestPassHeaders are the client headers the core hands accountType of
// the manifest, over all the platforms it serves (built-in or the
// manifest's own).
func manifestPassHeaders(t *testing.T, rawManifest []byte, accountType string) []string {
	t.Helper()
	var m manifest.Manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	known := append(platforms.Builtin(), m.Platforms...)
	lookup := func(id string) *manifest.Platform {
		for i := range known {
			if known[i].ID == id {
				return &known[i]
			}
		}
		return nil
	}
	for i := range m.AccountTypes {
		if m.AccountTypes[i].ID == accountType {
			return m.AccountTypes[i].PassHeaders(lookup)
		}
	}
	t.Fatalf("the manifest declares no account type %q", accountType)
	return nil
}

// Golden is the goldentest subject of p: Execute is p's BuildUpstream (what
// ExecuteDefault relays) on the client headers the core hands over, those
// of a case named in passHeaders (the file's PassHeaders; a case holds more
// on purpose); the other four kinds are p's own methods, classification at
// the case's clock.
func Golden(p *standard.Platform, passHeaders []string) goldentest.Subject {
	return goldentest.Subject{
		Execute: func(ctx context.Context, in *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
			built, err := p.BuildUpstream(ctx, &pluginv1.ExecuteRequest{
				Account: in.Account.Proto(), Meta: in.Meta.Proto(),
				InboundHeaders: passed(in.ClientHeaders, passHeaders), Body: []byte(in.Body),
			})
			if err != nil {
				return nil, err
			}
			return &goldentest.UpstreamRequest{
				Method: built.GetMethod(), URL: built.GetUrl(),
				Headers: built.GetHeaders(), DefaultHeaders: built.GetDefaultHeaders(),
				Body: string(built.GetBody()), UpstreamModel: built.GetUpstreamModel(),
			}, nil
		},
		Test:     p.BuildTestRequest,
		Models:   p.BuildModelsRequest,
		Validate: p.ValidateCredentials,
		Classify: func(_ context.Context, now time.Time, in *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
			return p.ClassifyAt(now, in), nil
		},
	}
}

// passed is the core's filter: the client headers named in names.
func passed(client map[string]string, names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if value, ok := client[name]; ok {
			out[name] = value
		}
	}
	return out
}
