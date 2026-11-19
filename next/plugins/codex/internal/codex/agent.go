package codex

import (
	"cmp"
	"context"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// The client headers (User-Agent, originator) every request of an account
// sends to OpenAI: the upstream (HTTP and WebSocket), the account test, the
// token refresh, the usage resets and the authorizations. Each is the
// account's own value (credentials user_agent / originator), else the
// plugin settings' value, else the built-in one (UserAgent, Originator). An
// empty value sets nothing and the next level applies. An authorization
// runs before the account is saved: its account values are the form's, which
// each route call carries (readCall).

// agentFields are the credential fields of the account's own values.
var agentFields = []string{"user_agent", "originator"}

// agent is the User-Agent and originator one account sends.
type agent struct {
	UserAgent  string
	Originator string
}

// agent resolves the client headers of the account with credentials c (for
// an authorization, the form's values).
func (p *Plugin) agent(c credentials) agent {
	s := p.settings.Load()
	return agent{
		UserAgent:  cmp.Or(c.UserAgent, s.UserAgent, UserAgent),
		Originator: cmp.Or(c.Originator, s.Originator, Originator),
	}
}

// Settings are the plugin settings (forms/settings.schema.json): the client
// headers of the accounts that set none, "" for the built-in ones. The host
// checks them against the schema before saving.
type Settings struct {
	UserAgent  string `json:"user_agent"`
	Originator string `json:"originator"`
}

// Configure implements pluginsdk.Configurer.
func (p *Plugin) Configure(_ context.Context, cfg pluginsdk.Config) error {
	var s Settings
	if err := cfg.Decode(&s); err != nil {
		return pluginsdk.FieldErrors{}.Add("", "invalid_json", "settings must be a JSON object / 设置必须是 JSON 对象").Err()
	}
	s.UserAgent, s.Originator = strings.TrimSpace(s.UserAgent), strings.TrimSpace(s.Originator)
	p.settings.Store(&s)
	return nil
}
