package core

import (
	"context"

	"github.com/Sub2API-Devs/sup2api/next/sdk/paramoverride"
)

// Request parameter override (CONTRACTS §84). The engine lives in
// sdk/paramoverride, where manifest/check validates declared upstreams with
// the same parser; these names keep the core's callers unchanged.

type (
	// ParamOverride is a compiled override. nil means none.
	ParamOverride = paramoverride.Override
	// ParamOverrideError is what a return_error operation answers the client.
	ParamOverrideError = paramoverride.Error
	// ParamOverrideContext are the request facts conditions can read.
	ParamOverrideContext = paramoverride.Context
	// ParamOverrideResult is the overridden body and the header decisions.
	ParamOverrideResult = paramoverride.Result
	// HeaderOverride is what the header operations decided.
	HeaderOverride = paramoverride.HeaderOverride
	// HeaderOverrideRules is a compiled request header override (CONTRACTS
	// §108). nil means none.
	HeaderOverrideRules = paramoverride.HeaderOverrideRules
)

const (
	// MaxParamOverrideBytes caps the serialized override of one account.
	MaxParamOverrideBytes = paramoverride.MaxBytes
	// MaxParamOverrideOperations caps its operations.
	MaxParamOverrideOperations = paramoverride.MaxOperations
)

// LoadParamOverride compiles a stored override. One that no longer compiles
// (it was validated when saved) fails every request of the account.
func LoadParamOverride(raw []byte) *ParamOverride { return paramoverride.Load(raw) }

// CompileParamOverride validates and compiles an override, with the messages
// in the request locale. null, {} and an override without operations or keys
// compile to nil (none).
func CompileParamOverride(ctx context.Context, field string, raw []byte) (*ParamOverride, []FieldError) {
	return compileLocalized(ctx, paramoverride.Compile, field, raw)
}

// LoadHeaderOverride compiles a stored request header override (§108). One
// that no longer compiles fails every request of the account.
func LoadHeaderOverride(raw []byte) *HeaderOverrideRules {
	return paramoverride.LoadHeaderOverride(raw)
}

// CompileHeaderOverride validates and compiles a request header override
// (§108), with the messages in the request locale. null and {} compile to
// nil (none).
func CompileHeaderOverride(ctx context.Context, field string, raw []byte) (*HeaderOverrideRules, []FieldError) {
	return compileLocalized(ctx, paramoverride.CompileHeaderOverride, field, raw)
}

// compileLocalized runs a compiler of sdk/paramoverride with the messages in
// the request locale; on errors the result is nil.
func compileLocalized[T any](ctx context.Context, compile func(string, []byte, bool) (*T, []paramoverride.FieldError), field string, raw []byte) (*T, []FieldError) {
	v, errs := compile(field, raw, Locale(ctx) == "zh")
	if len(errs) > 0 {
		return nil, fieldErrors(errs)
	}
	return v, nil
}

// fieldErrors converts the field errors of sdk/paramoverride; nil for none.
func fieldErrors(errs []paramoverride.FieldError) []FieldError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]FieldError, len(errs))
	for i, e := range errs {
		out[i] = FieldError(e)
	}
	return out
}
