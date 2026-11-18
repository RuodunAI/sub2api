package anthropic

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// APIKey is an Anthropic API key account type: x-api-key on
// https://api.anthropic.com (a pasted /v1 removed), anthropic-version
// unless the client sends one, messages and count_tokens, the Anthropic
// error policy, a one-token messages test and the model listing. It filters
// service_tier, inference_geo and speed, which belong to a client's own
// Anthropic account; a plugin that forwards them sets FilterFields to nil.
func APIKey(id string) standard.Type {
	return standard.Type{
		ID: id,
		Spec: apikey.Spec{
			DefaultBaseURL: "https://api.anthropic.com",
			StripSuffixes:  []string{"/v1"},
			Texts: apikey.Texts{
				UnsupportedAccountType: "unsupported account type {type} / 不支持的账号类型",
				KeyNotString:           "API key must be 8-512 printable characters without spaces / API Key 须为 8-512 个不含空格的可见字符",
				KeyNotStringCode:       "pattern",
				BaseURLFormat:          "base_url {reason} / base_url 必须是不含账号、查询参数的 http(s) 地址",
			},
		},
		PassHeaders: standard.PlatformPassHeaders("anthropic"),
		Auth:        standard.Auth{Header: "x-api-key"},
		Headers: []standard.Header{
			standard.Default("anthropic-version", standard.Fixed(DefaultAPIVersion)),
			standard.Set("content-type", standard.Fixed("application/json")),
		},
		Routes: map[string]standard.Route{
			ProtocolMessages:    {Path: standard.Path("/v1/messages")},
			ProtocolCountTokens: {Path: standard.Path("/v1/messages/count_tokens")},
		},
		DefaultProtocol: ProtocolMessages,
		FilterFields:    []string{"service_tier", "inference_geo", "speed"},
		Classify:        Policy,
		Test:            &standard.Test{Protocol: ProtocolMessages, Model: "claude-haiku-4-5", Body: func(m string) string { return MessagesPingBody(m, 1) }},
		Models:          &apikey.ModelList{Path: "/v1/models?limit=1000", IdsPath: "data.#.id"},
	}
}
