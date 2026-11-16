package core

import "fmt"

// Kinds of rows a scoped grant names (authz.ClearScopeGrants).
const (
	ScopeRefAccount = "account"
	ScopeRefGroup   = "group"
	ScopeRefProxy   = "proxy"
)

// scopeRefPatterns match the scopes naming a row of kind (CONTRACTS §21.0):
// account/X and group/G/account/X; group/G and group/G/account/X; proxy/X.
var scopeRefPatterns = map[string]string{
	ScopeRefAccount: `(^|/)account/%d$`,
	ScopeRefGroup:   `^group/%d(/|$)`,
	ScopeRefProxy:   `^proxy/%d$`,
}

// ScopeRefPatterns returns the regular expressions matching the scopes
// that name one of the rows ids of kind.
func ScopeRefPatterns(kind string, ids []int64) ([]string, error) {
	pattern, ok := scopeRefPatterns[kind]
	if !ok {
		return nil, fmt.Errorf("core: unknown scope reference kind %q", kind)
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf(pattern, id)
	}
	return out, nil
}
