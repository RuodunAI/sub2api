package check

import (
	"fmt"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"strings"
)

func (v *validator) additionalUsageRules(field string, rules []manifest.AdditionalUsageRule) {
	if len(rules) > 16 {
		v.add(field, "limit", "at most 16 additional usage rules")
	}
	names := map[string]bool{}
	selectors := map[string]bool{}
	for i, r := range rules {
		f := fmt.Sprintf("%s[%d]", field, i)
		if len(r.RequiredBy) > 8 || len(r.RequiredBy) > 0 && !r.UsePrimaryModel {
			v.add(f+".requiredBy", "invalid", "at most eight primary-model request requirements are supported")
		}
		paths := map[string]bool{}
		for _, path := range r.RequiredBy {
			if len(path) > 200 || !modelObjectPath.MatchString(path) || paths[path] {
				v.add(f+".requiredBy", "invalid", "unique simple object paths are required")
			}
			paths[path] = true
		}
		if !usageEventRe.MatchString(r.Name) || names[r.Name] {
			v.add(f+".name", "invalid", "additional usage name must be unique and bounded")
		}
		names[r.Name] = true
		if r.JSONPath == "" && r.SSEPath == "" {
			v.add(f, "required", "a JSON or SSE array path is required")
		}
		if r.Object {
			v.objectUsageRule(f, r)
		} else {
			v.arrayUsageRule(f, r)
		}
		if r.SSEEvent != "" && !usageEventRe.MatchString(r.SSEEvent) {
			v.add(f+".sseEvent", "invalid", "invalid SSE event")
		}
		if r.Semantics != "inclusive" && r.Semantics != "exclusive" {
			v.add(f+".semantics", "invalid", "additional semantics must be explicit")
		}
		v.usageMap(f+".map", r.Map, nil)
		for key, path := range r.Map {
			if key == manifest.UsageModel || strings.Contains(path, "+") {
				v.add(f+".map."+key, "invalid", "use modelPath; token counters must be individual exact integer paths")
			}
		}
		for _, key := range []string{manifest.UsageInputTokens, manifest.UsageOutputTokens} {
			if r.Map[key] == "" {
				v.add(f+".map."+key, "required", "input and output token mappings are required")
			}
		}
		for protocol, path := range []string{r.JSONPath, r.SSEPath} {
			if path == "" {
				continue
			}
			key := fmt.Sprint(protocol) + "|" + path + "|" + r.TypePath + "|" + r.TypeValue
			if selectors[key] {
				v.add(f, "duplicate", "same usage snapshot/type may not be billed twice")
			}
			selectors[key] = true
		}
	}
}

// arrayUsageRule checks the paths of a rule reading typed array entries.
func (v *validator) arrayUsageRule(f string, r manifest.AdditionalUsageRule) {
	if len(r.ModelSources) > 0 || r.DefaultModel != "" {
		v.add(f+".modelSources", "invalid", "model sources and a default model belong to object rules")
	}
	for key, path := range map[string]string{"jsonPath": r.JSONPath, "ssePath": r.SSEPath, "typePath": r.TypePath, "modelPath": r.ModelPath} {
		if key == "modelPath" && path == "" && r.UsePrimaryModel {
			continue
		}
		if path == "" && (key == "jsonPath" || key == "ssePath") {
			continue
		}
		v.usagePath(f+"."+key, path)
		if strings.Contains(path, "+") {
			v.add(f+"."+key, "invalid", "additional usage paths do not sum arrays or identities")
		}
	}
	if !usageEventRe.MatchString(r.TypeValue) {
		v.add(f+".typeValue", "invalid", "a bounded type value is required")
	}
}

// objectUsageRule checks a rule reading one usage object whose model the
// response or the request names (CONTRACTS §120).
func (v *validator) objectUsageRule(f string, r manifest.AdditionalUsageRule) {
	if r.UsePrimaryModel || r.TypePath != "" || r.TypeValue != "" || r.ModelPath != "" {
		v.add(f, "invalid", "an object rule has no primary model, type selector or entry model path")
	}
	for key, path := range map[string]string{"jsonPath": r.JSONPath, "ssePath": r.SSEPath} {
		if path == "" {
			continue
		}
		v.usagePath(f+"."+key, path)
		if strings.Contains(path, "+") {
			v.add(f+"."+key, "invalid", "additional usage paths do not sum objects")
		}
	}
	if len(r.ModelSources) == 0 && r.DefaultModel == "" {
		v.add(f+".modelSources", "required", "an object rule needs model sources or a default model")
	}
	if len(r.ModelSources) > 8 {
		v.add(f+".modelSources", "too_many", "at most 8 model sources are supported")
	}
	if len(r.DefaultModel) > 200 || strings.TrimSpace(r.DefaultModel) != r.DefaultModel || strings.ContainsAny(r.DefaultModel, "\r\n\x00") {
		v.add(f+".defaultModel", "invalid", "a model identifier up to 200 bytes is required")
	}
	for i, src := range r.ModelSources {
		at := fmt.Sprintf("%s.modelSources[%d]", f, i)
		switch src.From {
		case manifest.UsageModelFromJSON, manifest.UsageModelFromSSE, manifest.UsageModelFromRequest:
		default:
			v.add(at+".from", "invalid", "from must be json, sse or request")
		}
		if src.ArrayPath != "" && (len(src.ArrayPath) > 200 || !modelObjectPath.MatchString(src.ArrayPath)) {
			v.add(at+".arrayPath", "invalid", "a simple object path is required")
		}
		if len(src.ModelPath) > 200 || !modelObjectPath.MatchString(src.ModelPath) {
			v.add(at+".modelPath", "invalid", "a simple object path is required")
		}
		if len(src.Match) > 8 || len(src.Match) > 0 && src.ArrayPath == "" {
			v.add(at+".match", "invalid", "at most 8 match fields, on an array path")
		}
		for key, value := range src.Match {
			if len(key) > 200 || !modelObjectPath.MatchString(key) || value == "" || len(value) > 200 {
				v.add(at+".match", "invalid", "match requires simple paths and nonempty strings up to 200 bytes")
			}
		}
	}
}
