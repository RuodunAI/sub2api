package vertex

import (
	"context"
	"net/http"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/classify"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
)

// tokenCooldown keeps a key file account whose access token was refused out
// of scheduling while the core renews the token.
const tokenCooldown = time.Minute

// tokenPolicy classifies the failures of key file accounts: the Gemini
// policy, except that a 401 is about the short-lived token and not about the
// key, so the account cools down instead of being disabled. A key that
// really stopped working shows as auth_rejected in the refresh state.
var tokenPolicy = gemini.Policy.With(classify.Rule{
	When:     classify.Status(http.StatusUnauthorized),
	Effect:   classify.Cooldown,
	Cooldown: tokenCooldown,
	Reason:   "upstream rejected the access token (%d); the next credential refresh renews it",
})

// policyOf is the policy of an account type: API keys have nothing to renew
// and follow the Gemini policy as it is (401 and 403 disable).
func policyOf(typ string) classify.Policy {
	if _, keyed := keyKinds[typ]; keyed {
		return tokenPolicy
	}
	return gemini.Policy
}

// ClassifyError implements pluginsdk.Platform.
func (p *Plugin) ClassifyError(_ context.Context, in *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
	return policyOf(in.GetAccount().GetType()).Classify(p.now(), in), nil
}
