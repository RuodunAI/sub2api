package standard

import (
	"encoding/json"
	"net/url"
	"strings"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
)

// Request is what a path, a header value or an edit is computed from.
type Request struct {
	Account *pluginv1.Account
	// Meta is the gateway request's metadata; nil for the account test and
	// the model listing.
	Meta *pluginv1.RequestMeta
	// APIKey and BaseURL are the account's, normalized (apikey.Spec).
	APIKey  string
	BaseURL string
	// Model is the upstream model (Route.Model); "" for the model listing.
	Model string
	// Client are the forwarded client headers (Type.PassHeaders), lower-case
	// names, trimmed values; none for the account test and the listing.
	Client map[string]string

	fields []map[string]any
}

// Field reads an account field: the credentials first, then the settings;
// a string as it is, a number or a boolean as its JSON text, anything else
// (or nothing) as "".
func (r *Request) Field(name string) string {
	if r.fields == nil {
		// Both decoded once already (apikey.Spec.FromAccount), so they are
		// JSON objects.
		creds, _ := apikey.DecodeObject(r.Account.GetCredentialsJson())
		settings, _ := apikey.DecodeObject(r.Account.GetSettingsJson())
		r.fields = []map[string]any{creds, settings}
	}
	switch v, _ := apikey.Lookup(name, r.fields...); v := v.(type) {
	case string:
		return v
	case float64, bool:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return ""
}

// url is the URL of route on the base URL: path, the key in the query when
// the type puts it there, the ws(s) scheme of a websocket route.
func (t *Type) url(path string, websocket bool, r *Request) string {
	u := r.BaseURL + path
	if t.Auth.Query != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + url.QueryEscape(t.Auth.Query) + "=" + url.QueryEscape(t.Auth.Prefix+r.APIKey)
	}
	if websocket {
		u = "ws" + strings.TrimPrefix(u, "http") // the base URL is http(s) (apikey.NormalizeBaseURL)
	}
	return u
}

// headers are the request's default headers: the forwarded client headers,
// then the type's writes, then the route's. The credential is not among
// them (credential).
func (t *Type) headers(route []Header, r *Request) map[string]string {
	h := make(map[string]string, len(r.Client)+len(t.Headers)+len(route))
	for k, v := range r.Client {
		h[k] = v
	}
	for _, w := range t.Headers {
		w.write(h, r)
	}
	for _, w := range route {
		w.write(h, r)
	}
	return h
}

// credential is the authoritative header of the request: the key, when the
// type sends it in a header.
func (t *Type) credential(r *Request) map[string]string {
	if t.Auth.Header == "" {
		return map[string]string{}
	}
	return map[string]string{t.Auth.Header: t.Auth.Prefix + r.APIKey}
}

func (w Header) write(h map[string]string, r *Request) {
	value := w.Value(r)
	if value == "" {
		return
	}
	cur := h[w.Name]
	switch {
	case w.Mode == Replace, cur == "":
		h[w.Name] = value
	case w.Mode == Append && !hasToken(cur, value):
		h[w.Name] = cur + "," + value
	}
}

// hasToken reports whether the comma-separated list cur holds value; for a
// "key=version" value any version of key counts.
func hasToken(cur, value string) bool {
	key, _, versioned := strings.Cut(value, "=")
	for _, t := range strings.Split(cur, ",") {
		t = strings.TrimSpace(t)
		if t == value || (versioned && strings.HasPrefix(t, key+"=")) {
			return true
		}
	}
	return false
}

// forward are the client headers of inbound the type passes, trimmed,
// blank ones dropped.
func (t *Type) forward(inbound map[string]string) map[string]string {
	out := map[string]string{}
	apikey.ForwardHeaders(out, inbound, t.PassHeaders)
	return out
}

// normalizeModel removes surrounding spaces and prefix.
func normalizeModel(name, prefix string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), prefix)
}
