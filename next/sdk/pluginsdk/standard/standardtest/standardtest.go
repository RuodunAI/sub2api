// Package standardtest connects a standard.Platform to the frozen golden
// cases (sdk/pluginsdk/standard/goldentest): a plugin built on the standard
// library replays them with
//
//	standardtest.Run(t, "openai", myplugin.New())
package standardtest

import (
	"context"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
)

// Run replays every golden case of plugin against p (goldentest.Run with
// Golden).
func Run(t *testing.T, plugin string, p *standard.Platform, opts ...goldentest.Option) {
	t.Helper()
	f, err := goldentest.Load(plugin)
	if err != nil {
		t.Fatal(err)
	}
	goldentest.Run(t, plugin, Golden(p, f.PassHeaders), opts...)
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
