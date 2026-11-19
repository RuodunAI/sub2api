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
//	GET  /api/status              New API: the site's quota_per_unit (public)
//	GET  /v1/usage                Sub2API: the key's balance (Bearer)
//	POST /v1/images/generations   OpenAI image generation, JSON or SSE
//	POST /v1/images/edits         the same with the image_edit events
//	GET  /v1/models on Host api.typesafe.ai: TypeSafe's {"models":[{"name":...}]}
//
// A balance answer is a default (below) or a control rule's balance body;
// a rule's status answers that error instead. /api/status has no key: it
// always answers quota_per_unit 500000.

// typesafeHost is the vendor host name of TypeSafe (an e2e network alias).
const typesafeHost = "api.typesafe.ai"

// Default balance answers: 1250000 site units at 500000 per dollar (2.5
// USD) for New API, 7.5 USD of a legacy Sub2API key's quota.
var (
	defaultNewAPITokenUsage = json.RawMessage(`{"code":true,"message":"ok","data":{"object":"token_usage","name":"mock","total_granted":2000000,"total_used":750000,"total_available":1250000,"unlimited_quota":false,"expires_at":0}}`)
	newAPIStatus            = map[string]any{"success": true, "message": "", "data": map[string]any{"quota_per_unit": 500000, "system_name": "mock"}}
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
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		s.setStatus(s.record(r, nil, "", false, ""), 200)
		writeJSON(w, 200, newAPIStatus)
	})
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
