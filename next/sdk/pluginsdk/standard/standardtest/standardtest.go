// Package standardtest connects a standard.Platform to the frozen golden
// cases (sdk/pluginsdk/standard/goldentest): a plugin built on the standard
// library replays them with
//
//	goldentest.Run(t, "openai", standardtest.Golden(myplugin.New()))
package standardtest

import (
	"context"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
)

// Golden is the goldentest subject of p: Execute is p's BuildUpstream (what
// ExecuteDefault relays), the other four kinds are p's own methods,
// classification at the case's clock.
func Golden(p *standard.Platform) goldentest.Subject {
	return goldentest.Subject{
		Execute: func(ctx context.Context, in *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
			built, err := p.BuildUpstream(ctx, &pluginv1.ExecuteRequest{
				Account: in.Account.Proto(), Meta: in.Meta.Proto(),
				InboundHeaders: in.ClientHeaders, Body: []byte(in.Body),
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
