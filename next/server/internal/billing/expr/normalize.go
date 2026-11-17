package expr

// Usage semantics declared by platform plugins (manifest usage.semantics).
const (
	SemanticsExclusive = "exclusive" // input tokens exclude cache (Anthropic)
	SemanticsInclusive = "inclusive" // prompt tokens include cache (OpenAI)
)

// Tokens are raw upstream token counts. CacheCreation and CacheCreation1h
// are disjoint (5-minute and 1-hour cache writes).
type Tokens struct {
	Input           int64
	Output          int64
	CacheRead       int64
	CacheCreation   int64
	CacheCreation1h int64
}

// Normalize maps raw tokens to expression variables (ARCHITECTURE 7.3):
//
//   - len is the full context: input + cache (exclusive) or input (inclusive)
//   - every cache category the expression prices (cr, cc, cc1h) gets its own
//     count; a 1-hour cache write falls back to cc when only cc is priced
//   - p is the input without the cache tokens counted apart, never negative.
//     Unpriced cache reads are not billed. Unpriced cache writes are not
//     billed under exclusive semantics; under inclusive semantics they stay
//     in p, being input the upstream merely reports apart (OpenAI).
//
// usedVars is typically Program.Uses.
func Normalize(semantics string, t Tokens, uses func(string) bool) Vars {
	v := Vars{C: float64(t.Output), Len: float64(t.Input)}
	if uses("cr") {
		v.CR = float64(t.CacheRead)
	}
	switch {
	case uses("cc1h"):
		v.CC1h = float64(t.CacheCreation1h)
		if uses("cc") {
			v.CC = float64(t.CacheCreation)
		}
	case uses("cc"):
		v.CC = float64(t.CacheCreation + t.CacheCreation1h)
	}
	if semantics == SemanticsInclusive {
		v.P = max(float64(t.Input)-float64(t.CacheRead)-v.CC-v.CC1h, 0)
	} else {
		v.P = float64(t.Input)
		v.Len += float64(t.CacheRead + t.CacheCreation + t.CacheCreation1h)
	}
	return v
}
