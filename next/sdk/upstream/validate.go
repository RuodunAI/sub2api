package upstream

import (
	"time"

	"github.com/tidwall/sjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/paramoverride"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/classify"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
)

// policies are the error policies upstream.classify names.
var policies = map[string]classify.Policy{
	"openai":    openai.Policy,
	"anthropic": anthropic.Policy,
	"gemini":    gemini.Policy,
}

// pings are the built-in test bodies upstream.test.body names: the
// providers' one-token probes every plugin of that family sends.
var pings = map[string]func(model string) string{
	"openai.chat_ping": func(m string) string { return openai.ChatPingBody(m, 1) },
	"anthropic.ping":   func(m string) string { return anthropic.MessagesPingBody(m, 1) },
	"gemini.ping":      func(string) string { return gemini.GenerateContentPingBody(1) },
}

// Validate implements ValidateCredentials with apikey.Spec.Validate.
func (p *Profile) Validate(in *pluginv1.ValidateCredentialsRequest) *pluginv1.ValidateCredentialsResponse {
	return p.spec.Validate(in)
}

// Classify implements ClassifyError with the declared provider policy.
func (p *Profile) Classify(now time.Time, in *pluginv1.ClassifyErrorRequest) *pluginv1.ClassifyErrorResponse {
	return p.policy.Classify(now, in)
}

// PrepareBody is the declaration's part of the request body that precedes
// Build's patches (§4.3): the filterFields are removed, then the manifest's
// paramOverride runs (its header decisions are in the result). The core
// calls it after model mapping and protocol conversion; the account's own
// §84 override runs last, after every patch.
func (p *Profile) PrepareBody(body []byte, ctx *paramoverride.Context) (paramoverride.Result, error) {
	for _, f := range p.up.FilterFields {
		if out, err := sjson.DeleteBytes(body, f); err == nil {
			body = out // f is a plain key (checked): a literal path
		}
	}
	return p.override.Apply(body, ctx)
}
