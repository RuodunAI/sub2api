package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Upstream sites and OpenAI image generation (CONTRACTS §52, §118, §119):
//
//	GET  /api/usage/token/        New API: the key's quota (Bearer)
//	GET  /v1/usage                Sub2API: the key's balance (Bearer)
//	POST /v1/images/generations   OpenAI image generation, JSON or SSE
//	POST /v1/images/edits         the same with the image_edit events
//	GET  /v1/models on Host api.typesafe.ai: TypeSafe's {"models":[{"name":...}]}
//
// A balance answer is a default (below) or a control rule's balance body;
// a rule's status answers that error instead.
//
// A Responses request offering the image_generation tool gets the tool's
// usage and echo (imageToolOf, below).

// typesafeHost is the vendor host name of TypeSafe (an e2e network alias).
const typesafeHost = "api.typesafe.ai"

// Default balance answers: 1250000 site quota units for New API, 7.5 USD
// of a legacy Sub2API key's quota.
var (
	defaultNewAPITokenUsage = json.RawMessage(`{"code":true,"message":"ok","data":{"object":"token_usage","name":"mock","total_granted":2000000,"total_used":750000,"total_available":1250000,"unlimited_quota":false,"expires_at":0}}`)
	defaultSub2APIUsage     = json.RawMessage(`{"isValid":true,"mode":"quota_limited","quota":{"limit":10,"used":2.5,"remaining":7.5,"unit":"USD"},"remaining":7.5,"unit":"USD"}`)
)

// The usage of an image answer: JSON reports the text and
// image shares of input and output, the completed stream event only the
// image input share and the output (all image).
var (
	imageJSONUsage = map[string]any{
		"input_tokens": 50, "input_tokens_details": map[string]any{"image_tokens": 40, "text_tokens": 10},
		"output_tokens": 4200, "output_tokens_details": map[string]any{"image_tokens": 4000, "text_tokens": 200},
		"total_tokens": 4250,
	}
	imageStreamUsage = map[string]any{
		"input_tokens": 50, "input_tokens_details": map[string]any{"image_tokens": 40},
		"output_tokens": 4000, "total_tokens": 4050,
	}
)

// mockImage is the base64 image of every answer ("mock").
const mockImage = "bW9jaw=="

func (s *server) siteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage/token/", s.balanceAnswer(defaultNewAPITokenUsage))
	mux.HandleFunc("GET /v1/usage", s.balanceAnswer(defaultSub2APIUsage))
	mux.HandleFunc("POST /v1/images/generations", s.images("image_generation"))
	mux.HandleFunc("POST /v1/images/edits", s.images("image_edit"))
}

// balanceAnswer answers a balance request with the key's rule (its status
// or balance body) or def.
func (s *server) balanceAnswer(def json.RawMessage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.record(r, nil, "", false, "")
		b := s.resolve(r, nil)
		if b.status >= 400 {
			s.setStatus(id, b.status)
			writeOpenAIMockError(w, b.status)
			return
		}
		s.setStatus(id, 200)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(orDefault(b.balance, def))
	}
}

// images answers the image endpoint whose stream events start with prefix.
func (s *server) images(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		req, ok := s.parseOpenAI(w, r, body)
		if !ok {
			return
		}
		id, b, ok := s.begin(w, r, body, req.Model, req.Stream, req.Metadata, formatOpenAI)
		if !ok {
			return
		}
		s.setStatus(id, 200)
		created := time.Now().Unix()
		if !req.Stream {
			writeJSON(w, 200, map[string]any{"created": created, "data": []any{map[string]any{"b64_json": mockImage}}, "usage": imageJSONUsage})
			return
		}
		sw := startSSE(w, r, b.chunkDelay)
		partial, completed := prefix+".partial_image", prefix+".completed"
		if sw.send(partial, map[string]any{"type": partial, "b64_json": mockImage, "partial_image_index": 0, "created_at": created}) {
			sw.send(completed, map[string]any{"type": completed, "b64_json": mockImage, "created_at": created, "usage": imageStreamUsage})
		}
	}
}

// typesafeModels answers GET /v1/models on TypeSafe's host in its shape.
func (s *server) typesafeModels(w http.ResponseWriter, r *http.Request) {
	s.setStatus(s.record(r, nil, "", false, ""), 200)
	writeJSON(w, 200, map[string]any{"models": []any{
		map[string]any{"name": "jev-1.14.0", "description": "mock Jev", "release_date": "2026-10-14"},
	}})
}

// isTypeSafe reports a request sent to TypeSafe's host name.
func isTypeSafe(r *http.Request) bool {
	host, _, _ := strings.Cut(r.Host, ":")
	return host == typesafeHost
}

// orDefault is v, or def when v is empty.
func orDefault(v, def json.RawMessage) json.RawMessage {
	if len(v) > 0 {
		return v
	}
	return def
}

// imageToolEchoModel is the image model the tools echo of a Responses
// answer names, as the Codex backend's does. The core never prices it
// (CONTRACTS §118.10).
const imageToolEchoModel = "gpt-image-2-codex"

// imageToolUsage is response.tool_usage.image_gen of a turn using the tool.
var imageToolUsage = map[string]any{
	"input_tokens": 39, "input_tokens_details": map[string]any{"text_tokens": 39, "image_tokens": 0},
	"output_tokens": 515, "output_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 515},
	"total_tokens": 554,
}

// imageToolOf reports whether a Responses request body offers the
// image_generation tool, and the tools echo of its final response.
func imageToolOf(body []byte) ([]any, bool) {
	var req struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	for _, t := range req.Tools {
		if t.Type != "image_generation" {
			continue
		}
		return []any{map[string]any{"type": "image_generation", "model": imageToolEchoModel, "quality": "auto", "size": "auto"}}, true
	}
	return nil, false
}

// withImageTool adds the tools echo and the tool's usage to a final
// response object.
func withImageTool(final map[string]any, tools []any) map[string]any {
	final["tools"] = tools
	final["tool_usage"] = map[string]any{"image_gen": imageToolUsage}
	return final
}

// imageCallItem is the output item of the image the tool made.
func imageCallItem(id string) map[string]any {
	return map[string]any{"type": "image_generation_call", "id": id, "status": "completed", "result": mockImage}
}
