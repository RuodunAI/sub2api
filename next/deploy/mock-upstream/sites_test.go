package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getReq(t *testing.T, srv *httptest.Server, path, host string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func setRule(t *testing.T, srv *httptest.Server, rule string) {
	t.Helper()
	if resp := post(t, srv, "/__control", nil, rule); resp.StatusCode != 200 {
		t.Fatalf("rule %s: %d", rule, resp.StatusCode)
	}
}

func TestBalances(t *testing.T) {
	s, srv := newTestServer(t)
	bearer := map[string]string{"Authorization": "Bearer sk-site-1"}

	// New API: the default token usage, the public status without a key.
	m := decode(t, getReq(t, srv, "/api/usage/token/", "", bearer))
	if get(m, "code") != true || num(get(m, "data", "total_available")) != 1250000 || get(m, "data", "unlimited_quota") != false {
		t.Fatalf("token usage = %v", m)
	}
	m = decode(t, getReq(t, srv, "/api/status", "", nil))
	if get(m, "success") != true || num(get(m, "data", "quota_per_unit")) != 500000 {
		t.Fatalf("status = %v", m)
	}
	s.mu.Lock()
	rec := s.log[len(s.log)-1]
	s.mu.Unlock()
	if rec.Path != "/api/status" || rec.Authorization != "" || rec.Status != 200 {
		t.Fatalf("recorded status = %+v", rec)
	}

	// Sub2API: the legacy quota shape by default.
	m = decode(t, getReq(t, srv, "/v1/usage", "", bearer))
	if get(m, "isValid") != true || get(m, "remaining") != 7.5 || get(m, "unit") != "USD" || get(m, "quota", "remaining") != 7.5 {
		t.Fatalf("usage = %v", m)
	}

	// A rule's balance replaces the answer; its status answers an error.
	setRule(t, srv, `{"api_key":"sk-site-1","balance":{"isValid":true,"remaining":-1,"unit":"USD"}}`)
	m = decode(t, getReq(t, srv, "/v1/usage", "", bearer))
	if get(m, "remaining") != -1.0 || get(m, "quota") != nil {
		t.Fatalf("usage by rule = %v", m)
	}
	setRule(t, srv, `{"api_key":"sk-site-1","status":401}`)
	if resp := getReq(t, srv, "/api/usage/token/", "", bearer); resp.StatusCode != 401 {
		t.Fatalf("token usage by rule = %d", resp.StatusCode)
	}
}

func TestImages(t *testing.T) {
	_, srv := newTestServer(t)
	auth := map[string]string{"Authorization": "Bearer sk-img-1", "x-mock-chunk-delay-ms": "1"}
	m := decode(t, post(t, srv, "/v1/images/generations", auth, `{"model":"gpt-image-1","prompt":"a cat"}`))
	if get(m, "data").([]any)[0].(map[string]any)["b64_json"] != mockImage || num(get(m, "usage", "input_tokens")) != 50 ||
		num(get(m, "usage", "input_tokens_details", "image_tokens")) != 40 || num(get(m, "usage", "output_tokens")) != 4200 ||
		num(get(m, "usage", "output_tokens_details", "image_tokens")) != 4000 {
		t.Fatalf("generations = %v", m)
	}
	for path, prefix := range map[string]string{"/v1/images/generations": "image_generation", "/v1/images/edits": "image_edit"} {
		events, data := sseData(t, post(t, srv, path, auth, `{"model":"gpt-image-1","prompt":"a cat","stream":true}`))
		if len(events) != 2 || events[0] != prefix+".partial_image" || events[1] != prefix+".completed" {
			t.Fatalf("%s events = %v", path, events)
		}
		var partial, done map[string]any
		_ = json.Unmarshal([]byte(data[0]), &partial)
		_ = json.Unmarshal([]byte(data[1]), &done)
		if partial["usage"] != nil || get(done, "type") != prefix+".completed" || num(get(done, "usage", "output_tokens")) != 4000 ||
			num(get(done, "usage", "input_tokens_details", "image_tokens")) != 40 {
			t.Fatalf("%s data = %v", path, data)
		}
	}
	if resp := post(t, srv, "/v1/images/edits", map[string]string{"Authorization": "Bearer k", "x-mock-status": "500"}, `{"model":"m"}`); resp.StatusCode != 500 {
		t.Fatalf("edits error = %d", resp.StatusCode)
	}
}

func TestTypeSafe(t *testing.T) {
	_, srv := newTestServer(t)
	bearer := map[string]string{"Authorization": "Bearer ts-1"}
	m := decode(t, getReq(t, srv, "/v1/models", typesafeHost, bearer))
	if get(m, "models").([]any)[0].(map[string]any)["name"] != "jev-1.14.0" || get(m, "data") != nil {
		t.Fatalf("typesafe models = %v", m)
	}
	// Elsewhere the list keeps the OpenAI shape.
	m = decode(t, getReq(t, srv, "/v1/models", "", bearer))
	if get(m, "data") == nil || get(m, "models") != nil {
		t.Fatalf("models = %v", m)
	}
	j := decode(t, post(t, srv, "/v1/systemone", bearer, `{"model":"jev-latest","state":"x","questions":{"q":{}}}`))
	if get(j, "model") != "jev-latest" || num(get(j, "usage", "input_tokens")) != 120 || get(j, "usage", "output_tokens") != 0.0 {
		t.Fatalf("systemone = %v", j)
	}
	if resp := post(t, srv, "/v1/systemone", map[string]string{"Authorization": "Bearer ts-x-status-429"}, `{"model":"m"}`); resp.StatusCode != 429 ||
		!strings.Contains(resp.Header.Get("Content-Type"), "json") {
		t.Fatalf("systemone error = %d", resp.StatusCode)
	}
}
