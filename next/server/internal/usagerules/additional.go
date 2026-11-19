package usagerules

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/tidwall/gjson"
)

const MaxAdditionalItems = 128
const maxAdditionalTokens int64 = 1_000_000_000_000

// WithPrimaryModel must receive the host-admitted, account-mapped model,
// never an untrusted response's top-level model field.
func (u *Acc) WithPrimaryModel(model string) *Acc { u.primaryModel = model; return u }

func (u *Acc) Additional() []core.AdditionalUsage {
	var out []core.AdditionalUsage
	for _, rule := range u.rules.Additional {
		for _, item := range u.additional[rule.Name] {
			item.Metrics = core.CloneUsageMetrics(item.Metrics)
			out = append(out, item)
		}
	}
	return out
}

// WithRequest gives the accumulator the request as it went upstream, which
// the "request" model sources of object rules read (CONTRACTS §120). The
// body is only read, never kept beyond the accumulator.
func (u *Acc) WithRequest(body []byte) *Acc { u.request = body; return u }

func (u *Acc) applyAdditional(event string, body []byte, stream bool) {
	for _, rule := range u.rules.Additional {
		path := rule.JSONPath
		if stream {
			if rule.SSEEvent != "" && rule.SSEEvent != event {
				continue
			}
			path = rule.SSEPath
		}
		if path == "" {
			continue
		}
		snapshot := gjson.GetBytes(body, path)
		if !snapshot.Exists() || snapshot.Type == gjson.Null {
			continue
		}
		var entries []core.AdditionalUsage
		var err error
		if rule.Object {
			entries, err = u.objectEntries(rule, snapshot, body, stream)
		} else {
			entries, err = u.arrayEntries(rule, snapshot)
		}
		if err != nil {
			u.AdditionalError = fmt.Sprintf("additional usage %s: %v", rule.Name, err)
			continue
		}
		if u.additional == nil {
			u.additional = map[string][]core.AdditionalUsage{}
		}
		u.additional[rule.Name] = entries
	}
	if len(u.Additional()) > MaxAdditionalItems {
		u.AdditionalError = "additional usage total exceeds limit"
	}
}

// arrayEntries reads the entries of an array snapshot whose type matches.
func (u *Acc) arrayEntries(rule manifest.AdditionalUsageRule, array gjson.Result) ([]core.AdditionalUsage, error) {
	if !array.IsArray() || len(array.Array()) > MaxAdditionalItems {
		return nil, fmt.Errorf("invalid or oversized snapshot")
	}
	var entries []core.AdditionalUsage
	for _, entry := range array.Array() {
		if entry.Get(rule.TypePath).String() != rule.TypeValue {
			continue
		}
		modelPath := rule.ModelPath
		if modelPath == "" && rule.UsePrimaryModel {
			modelPath = "model"
		}
		model := entry.Get(modelPath)
		if rule.UsePrimaryModel {
			if model.Exists() && (model.Type != gjson.String || model.Str != u.primaryModel) {
				return nil, fmt.Errorf("response changed the admitted primary model")
			}
			model = gjson.Result{Type: gjson.String, Str: u.primaryModel}
		}
		if !validModel(model) {
			return nil, fmt.Errorf("invalid model")
		}
		tokens, err := additionalTokens(rule.Map, entry, true)
		if err != nil {
			return nil, err
		}
		entries = append(entries, core.AdditionalUsage{Metrics: additionalCacheMetrics(rule.Map, entry), Kind: rule.Name, Model: model.Str, UsageSemantics: rule.Semantics, Tokens: tokens})
	}
	return entries, nil
}

// objectEntries reads an object snapshot (CONTRACTS §120): no entry when
// every counter is 0 or absent, else one whose model the rule's sources name
// in body (the document holding the object) or the request, or the rule's
// default. The model was not admitted before scheduling: the gateway prices
// it when it records the usage.
func (u *Acc) objectEntries(rule manifest.AdditionalUsageRule, object gjson.Result, body []byte, stream bool) ([]core.AdditionalUsage, error) {
	if !object.IsObject() {
		return nil, fmt.Errorf("invalid snapshot")
	}
	tokens, err := additionalTokens(rule.Map, object, false)
	if err != nil || tokens == (core.UsageTokens{}) {
		return nil, err
	}
	model, err := u.objectModel(rule, body, stream)
	if err != nil {
		return nil, err
	}
	return []core.AdditionalUsage{{Kind: rule.Name, Model: model, UsageSemantics: rule.Semantics, Tokens: tokens, ResponseModel: true}}, nil
}

// objectModel is the first model a source of rule names, else the default.
// Sources of the other response form are skipped.
func (u *Acc) objectModel(rule manifest.AdditionalUsageRule, body []byte, stream bool) (string, error) {
	response := manifest.UsageModelFromJSON
	if stream {
		response = manifest.UsageModelFromSSE
	}
	for _, src := range rule.ModelSources {
		doc := body
		switch src.From {
		case response:
		case manifest.UsageModelFromRequest:
			doc = u.request
		default:
			continue
		}
		model := locateModel(gjson.ParseBytes(doc), src)
		if !model.Exists() {
			continue
		}
		if !validModel(model) {
			return "", fmt.Errorf("invalid model at %s", src.From)
		}
		return model.Str, nil
	}
	if rule.DefaultModel == "" {
		return "", fmt.Errorf("no model named")
	}
	return rule.DefaultModel, nil
}

// locateModel reads src.ModelPath in root, or in the first object of the
// array at src.ArrayPath matching src.Match.
func locateModel(root gjson.Result, src manifest.UsageModelSource) gjson.Result {
	if src.ArrayPath == "" {
		return root.Get(src.ModelPath)
	}
	var model gjson.Result
	root.Get(src.ArrayPath).ForEach(func(_, item gjson.Result) bool {
		if Matches(item, src.Match) {
			model = item.Get(src.ModelPath)
			return false
		}
		return true
	})
	return model
}

// Matches reports whether item is an object whose fields at the paths of
// match are the given strings.
func Matches(item gjson.Result, match map[string]string) bool {
	if !item.IsObject() {
		return false
	}
	for path, expected := range match {
		value := item.Get(path)
		if value.Type != gjson.String || value.Str != expected {
			return false
		}
	}
	return true
}

func validModel(model gjson.Result) bool {
	return model.Type == gjson.String && len(model.Str) > 0 && utf8.ValidString(model.Str) && !strings.ContainsAny(model.Str, "\r\n\x00")
}

// additionalTokens reads the counters mapping declares in entry. An array
// entry must report input and output (required); an object counts an absent
// counter as 0.
func additionalTokens(mapping map[string]string, entry gjson.Result, required bool) (core.UsageTokens, error) {
	values := map[string]int64{}
	for field, path := range mapping {
		value := gjson.Get(entry.Raw, path)
		if !value.Exists() {
			if required && (field == "input_tokens" || field == "output_tokens") {
				return core.UsageTokens{}, fmt.Errorf("missing token counter %s", field)
			}
			continue
		}
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		if value.Type != gjson.Number || err != nil || n < 0 || n > maxAdditionalTokens {
			return core.UsageTokens{}, fmt.Errorf("invalid token counter %s", field)
		}
		values[field] = n
	}
	if values["cache_creation_1h_tokens"] > values["cache_creation_tokens"] {
		return core.UsageTokens{}, fmt.Errorf("cache creation subdivision exceeds total")
	}
	t := Tokens(values["input_tokens"], values["output_tokens"], values["cache_read_tokens"], values["cache_creation_tokens"], values["cache_creation_1h_tokens"])
	t.ImageInput, t.ImageOutput = values[manifest.UsageImageInputTokens], values[manifest.UsageImageOutputTokens]
	return t, nil
}

func additionalCacheMetrics(mapping map[string]string, entry gjson.Result) map[string]any {
	if mapping["cache_creation_tokens"] != "cache_creation_input_tokens" || mapping["cache_creation_1h_tokens"] != "cache_creation.ephemeral_1h_input_tokens" {
		return nil
	}
	return iterationCacheMetrics(entry)
}
