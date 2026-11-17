// Package upstream is the engine of declared upstreams
// (docs/UPSTREAM-DECLARATIVE.md §3): it resolves an account type's
// accountTypes[].upstream against its core preset and builds what a platform
// plugin would answer - credential validation, the upstream request, the test
// and model listing requests, the error classification - from the
// declaration alone. The core runs it in place of a plugin process; the
// packaging CLI and plugin tests run the same code through manifest/check,
// so a declaration that checks here behaves the same in the core.
//
// It is pure computation over the plugin protocol messages: no I/O, no
// state beyond the resolved declaration.
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/paramoverride"
	presets "github.com/Sub2API-Devs/sup2api/next/sdk/platforms/upstream"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/classify"
)

// Profile is a resolved declaration: preset and manifest merged, checked
// and ready to build requests. It is immutable and safe for concurrent use.
type Profile struct {
	accountType string
	passHeaders []string
	up          manifest.Upstream
	spec        apikey.Spec
	policy      classify.Policy
	override    *paramoverride.Override
}

// Resolve merges the preset named by at.Upstream and the declaration itself,
// and checks the result. passHeaders are the client headers the account
// type receives (accountTypes[].platforms[].passHeaders, else the
// platforms'): the request carries those only, and {client_header.<name>}
// may read them. Problems are reported as Problems.
func Resolve(at *manifest.AccountType, passHeaders []string) (*Profile, error) {
	if at == nil || at.Upstream == nil {
		return nil, Problems{{Path: "", Code: "required", Message: "the account type declares no upstream"}}
	}
	if at.Upstream.Preset == "" {
		return nil, Problems{{Path: "preset", Code: "required", Message: "preset is required (openai, anthropic or gemini)"}}
	}
	preset, ok := presets.Preset(at.Upstream.Preset)
	if !ok {
		return nil, Problems{{Path: "preset", Code: "unknown", Message: fmt.Sprintf("unknown preset %q (openai, anthropic or gemini)", at.Upstream.Preset)}}
	}
	layer := map[string]any{}
	for _, raw := range []json.RawMessage{preset.Layer(), at.Upstream.Layer()} {
		patch, err := decodeObject(raw)
		if err != nil {
			return nil, Problems{{Code: "invalid", Message: "upstream must be a JSON object"}}
		}
		layer = merge(layer, patch, false)
	}
	p, probs := newProfile(at.ID, passHeaders, layer)
	if len(probs) == 0 {
		var errs []paramoverride.FieldError
		p.override, errs = paramoverride.Compile("paramOverride", p.up.ParamOverride, false)
		for _, e := range errs {
			probs = append(probs, Problem{Path: e.Field, Code: e.Code, Message: e.Message})
		}
	}
	if len(probs) > 0 {
		return nil, probs
	}
	return p, nil
}

// newProfile decodes and checks a merged layer.
func newProfile(accountType string, passHeaders []string, layer map[string]any) (*Profile, Problems) {
	b, _ := json.Marshal(layer)
	// A type without manifest.Upstream's UnmarshalJSON, so that unknown
	// fields (a misspelt key would silently change requests) are rejected.
	type strict manifest.Upstream
	var st strict
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, Problems{{Code: "invalid", Message: err.Error()}}
	}
	up := manifest.Upstream(st)
	if probs := check(&up, passHeaders); len(probs) > 0 {
		return nil, probs
	}
	return &Profile{
		accountType: accountType,
		passHeaders: passHeaders,
		up:          up,
		spec:        specOf(accountType, &up),
		policy:      policies[up.Classify],
	}, nil
}

// specOf is the apikey.Spec the declaration describes.
func specOf(accountType string, up *manifest.Upstream) apikey.Spec {
	s := apikey.Spec{AccountType: accountType}
	if b := up.BaseURL; b != nil {
		s.DefaultBaseURL, s.StripSuffixes, s.RequireBaseURL = b.Default, b.StripSuffixes, b.Required
	}
	if c := up.Credentials; c != nil {
		if kv := c.KeyValidator; kv != nil {
			s.KeyValidator = &apikey.Validator{Prefixes: kv.Prefixes, MinLength: kv.MinLength, Field: "api_key"}
		}
		if c.Texts != nil {
			s.Texts = apikey.Texts(*c.Texts)
		}
	}
	return s
}

// Upstream returns the resolved declaration. Its maps are the profile's:
// read them, do not modify them.
func (p *Profile) Upstream() manifest.Upstream { return p.up }

// AuthHeader is the lower-case name of the header auth writes (empty for
// query or no auth). The core writes it last, after every other header
// source, so nothing but the declaration decides the credential the
// upstream receives (§0.2, §4.3).
func (p *Profile) AuthHeader() string {
	if p.up.Auth == nil || p.up.Auth.Type != manifest.UpstreamAuthHeader {
		return ""
	}
	return strings.ToLower(p.up.Auth.Name)
}

// merge applies patch onto base like a JSON merge patch (RFC 7386): objects
// merge by key, null deletes, anything else replaces. A header write (an
// entry of a "headers" object) and paramOverride are replaced whole: their
// keys are a choice of mode, not fields to combine. A null header write is
// kept as a tombstone, so that a route's null also removes the shared write
// of that header when the two are combined (call.headers). base is not
// modified.
func merge(base, patch map[string]any, headers bool) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for k, pv := range patch {
		po, isObj := pv.(map[string]any)
		switch {
		case pv == nil && !headers:
			delete(out, k)
		case !isObj || headers || k == "paramOverride":
			out[k] = pv
		default:
			bo, _ := out[k].(map[string]any)
			out[k] = merge(bo, po, k == "headers")
		}
	}
	return out
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}
