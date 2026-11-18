package openai

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// Protocol ids of the built-in openai platform (sdk/platforms/openai.json).
const (
	ProtocolChat           = "openai.chat"
	ProtocolResponses      = "openai.responses"
	ProtocolResponsesWS    = "openai.responses_ws"
	ProtocolEmbeddings     = "openai.embeddings"
	ResponsesWebSocketBeta = "responses_websockets=2026-02-06" // openai-beta of the websocket handshake
)

// APIKey is an OpenAI API key account type: Bearer key on
// https://api.openai.com (a pasted /v1 removed), chat completions with the
// usage of a stream requested, responses over HTTP and websocket,
// embeddings, the OpenAI error policy, a one-token chat test and the model
// listing. It filters service_tier and safety_identifier, which belong to a
// client's own OpenAI account; a plugin that forwards them sets
// FilterFields to nil.
func APIKey(id string) standard.Type {
	return standard.Type{
		ID: id,
		Spec: apikey.Spec{
			DefaultBaseURL: "https://api.openai.com",
			StripSuffixes:  []string{"/v1"},
			KeyValidator:   &apikey.Validator{Prefixes: []string{"sk-", "sess-"}, MinLength: 10, Field: "api_key"},
			Texts: apikey.Texts{
				UnsupportedAccountType: "unsupported account type {type} / 不支持的账号类型",
				KeyNotString:           "API key must be a string / api_key 必须是字符串",
				KeyNotStringCode:       "pattern",
			},
		},
		PassHeaders: standard.PlatformPassHeaders("openai"),
		Auth:        standard.Auth{Header: "authorization", Prefix: "Bearer "},
		Headers:     []standard.Header{standard.Set("content-type", standard.Fixed("application/json"))},
		Routes: map[string]standard.Route{
			ProtocolChat:      {Path: standard.Path("/v1/chat/completions"), Edit: IncludeStreamUsage},
			ProtocolResponses: {Path: standard.Path("/v1/responses")},
			ProtocolResponsesWS: {
				Path:      standard.Path("/v1/responses"),
				WebSocket: true,
				Headers:   []standard.Header{standard.AppendTo("openai-beta", standard.Fixed(ResponsesWebSocketBeta))},
			},
			ProtocolEmbeddings: {Path: standard.Path("/v1/embeddings")},
		},
		DefaultProtocol: ProtocolChat,
		FilterFields:    []string{"service_tier", "safety_identifier"},
		Classify:        Policy,
		Test:            &standard.Test{Protocol: ProtocolChat, Model: "gpt-4o-mini", Body: func(m string) string { return ChatPingBody(m, 1) }},
		Models:          &apikey.ModelList{Path: "/v1/models", IdsPath: "data.#.id"},
	}
}

// IncludeStreamUsage is the edit of a chat completion: a stream asks for
// the usage chunk (stream_options.include_usage), which the host's usage
// rules read. It follows the request's meta.stream, not the body's flag.
func IncludeStreamUsage(body []byte, r *standard.Request) ([]byte, error) {
	if !r.Meta.GetStream() {
		return body, nil
	}
	return standard.SetJSON(body, "stream_options.include_usage", true)
}
