// Package newapi is the platform of New API sites: one apikey type serving
// the OpenAI, Anthropic, Gemini and TypeSafe protocols as they are, each
// with its official auth header, and the key's balance.
package newapi

import (
	"github.com/tidwall/gjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/typesafe"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// TypeAPIKey is the account type id (manifest.json accountTypes[0].id).
const TypeAPIKey = "apikey"

// BaseURLRequired is the message of an account without a base URL.
const BaseURLRequired = "Base URL of the New API site is required / 请填写 New API 站点的地址"

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(APIKey()) }

// APIKey is the four presets on the site's own address (required, a
// pasted /v1beta or /v1 removed, any 8-512 printable characters as the
// key), with the balance of the key in US dollars.
func APIKey() standard.Type {
	spec := apikey.Spec{
		RequireBaseURL: true,
		StripSuffixes:  []string{"/v1beta", "/v1"},
		Texts: apikey.Texts{
			UnsupportedAccountType: "unsupported account type {type} / 不支持的账号类型",
			KeyNotString:           "API key must be 8-512 printable characters without spaces / API Key 须为 8-512 个不含空格的可见字符",
			KeyNotStringCode:       "pattern",
			BaseURLRequired:        BaseURLRequired,
			BaseURLFormat:          "base_url {reason} / base_url 必须是不含账号、查询参数的 http(s) 地址",
		},
	}
	t := standard.Merge(TypeAPIKey, spec, openai.APIKey(""), anthropic.APIKey(""), gemini.APIKey(""), typesafe.APIKey(""))
	t.Balance = &standard.Balance{
		Requests: []standard.BalanceRequest{
			{Path: "/api/usage/token/"},                   // the key's quota, with the key
			{Path: "/api/status", Auth: &standard.Auth{}}, // the site's unit of account, public: no key
		},
		Read: readBalance,
	}
	return t
}

// readBalance reads the key's available quota (the lower of the key's and
// its user's when the site reports the user's; the user's for a key
// without a limit; no limit when neither has one) in the site's units, of
// which quota_per_unit make one US dollar. Without quota_per_unit there is
// no balance: the default of the site's software is never assumed.
func readBalance(_ *standard.Request, answers []*pluginv1.BalanceAnswer) *pluginv1.BalanceResult {
	for _, a := range answers {
		if f := standard.BalanceFailure(a); f != nil {
			return f
		}
	}
	token, site := gjson.ParseBytes(answers[0].GetBody()), gjson.ParseBytes(answers[1].GetBody())
	perUnit := site.Get("data.quota_per_unit").Float()
	switch {
	case !token.Get("code").Bool():
		return transient("token usage refused: " + token.Get("message").String() + " / 站点拒绝查询令牌额度")
	case perUnit <= 0:
		return transient("no quota_per_unit in /api/status / 无法读取站点的额度换算比例")
	}
	d, user := token.Get("data"), token.Get("data.user_quota.total_available")
	var available float64
	switch {
	case !d.Get("unlimited_quota").Bool():
		available = d.Get("total_available").Float()
		if user.Exists() {
			available = min(available, user.Float())
		}
	case user.Exists():
		available = user.Float()
	default:
		return &pluginv1.BalanceResult{Unlimited: true}
	}
	return &pluginv1.BalanceResult{AmountMicros: standard.USDMicros(available, perUnit), Currency: "USD"}
}

func transient(msg string) *pluginv1.BalanceResult {
	return &pluginv1.BalanceResult{ErrorType: pluginv1.BalanceResult_ERROR_TYPE_TRANSIENT, ErrorMessage: msg}
}
