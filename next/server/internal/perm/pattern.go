package perm

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// Scoped permission keys (CONTRACTS §21, wildcard permissions): an action
// key may end in a scope, "<action>:<scope>", that limits the rows it
// reaches:
//
//	scope = "*" | "own" | "team" | path
//	path  = account actions: "group/" seg [ "/account/" seg ] | "account/" id
//	        proxy actions:   "proxy/" id
//	        plugin actions:  resource "/" value
//	seg   = id | "*"
//
// "team" (CONTRACTS §103) is the caller's rows and those of every user
// below it (users.owner_id); only core actions take it, and of the user
// actions it is the only scope besides "*". A missing scope is "*": every
// row, so every unscoped key keeps its meaning. The last segment of a key
// is a scope only when it is "*", "own", holds a "/", or is "team" after a
// core action that takes a scope (so a plugin action ending in ":team"
// keeps its meaning); no catalog action ends that way.

// Resource is the row kind the scope of an action ranges over.
type Resource string

const (
	ResourceAccount Resource = "account"
	ResourceProxy   Resource = "proxy"
	// ResourceUser ranges over users by their owner (users.owner_id): "*"
	// or "team", which reaches the users below the holder, not itself.
	ResourceUser Resource = "user"
)

// scopable lists the actions that take a scope, with their resource.
var scopable = map[Key]Resource{
	AccountRead:           ResourceAccount,
	AccountCreate:         ResourceAccount,
	AccountUpdate:         ResourceAccount,
	AccountDelete:         ResourceAccount,
	AccountTest:           ResourceAccount,
	AccountCredentialView: ResourceAccount,
	ProxyRead:             ResourceProxy,
	ProxyManage:           ResourceProxy,
	UsageAccountRead:      ResourceAccount,
	UserRead:              ResourceUser,
	UserCreate:            ResourceUser,
	UserUpdate:            ResourceUser,
}

// PluginPrefix starts the action of every plugin permission
// (manifest.PermissionModule). Such an action takes the plugin scope
// grammar; whether it takes a scope at all, and which resource, own or
// values, is the catalog's to say (permissions.scope_type), checked by
// authz when a grant is written.
const PluginPrefix = "plugin."

// The plugin scope "<resource>/<value>", as a manifest declares it.
var (
	scopeResourceRe = regexp.MustCompile(manifest.ScopeResourcePattern)
	scopeValueRe    = regexp.MustCompile(manifest.ScopeValuePattern)
)

// ValidScopeValue reports whether v may be a plugin scope value (a row id
// or a values key); the runtime checks a scopeFrom value with it before
// building a key.
func ValidScopeValue(v string) bool { return scopeValueRe.MatchString(v) }

// ScopeType is the resource the scope of action ranges over, "" when the
// action takes no scope (the permissions.scope_type column).
func ScopeType(action Key) Resource { return scopable[action] }

// ErrInvalidKey: a scope that does not parse, on an action that takes none,
// or that the action does not accept.
var ErrInvalidKey = errors.New("invalid permission key")

// Wildcard is a group segment of "*": any group.
const Wildcard int64 = -1

// Scope is a parsed, canonical scope. At most one of All, Own and Team is
// set, and then the rest is zero. A plugin scope sets Res and Value only.
// Otherwise:
//   - Group > 0: group/<Group>; Group == Wildcard: group/*; 0: no group part.
//   - ID > 0: the row id, account/<ID> or proxy/<ID> (after a group part,
//     group/<g>/account/<ID>).
//
// Scope stays comparable: PermissionSet compares scopes with ==.
type Scope struct {
	All, Own   bool
	Team       bool // the holder's rows and those of the users below it
	Group, ID  int64
	Res, Value string // a plugin scope "<Res>/<Value>"
}

// Pattern is a canonical permission key split into action and scope; build
// it with Parse (a zero Scope is not a valid one).
// Unscoped keys (every action that takes no scope, plugin keys without a
// scope) have Scope.All set.
type Pattern struct {
	Action Key
	Scope  Scope
}

// Parse reads a key, trailing wildcards included, into its canonical
// pattern.
func Parse(k string) (Pattern, error) {
	i := strings.LastIndexByte(k, ':')
	if i < 0 {
		return Pattern{Action: Key(k), Scope: Scope{All: true}}, nil
	}
	action, last := Key(k[:i]), k[i+1:]
	res := scopable[action]
	if last != "*" && last != "own" && !strings.Contains(last, "/") && (last != "team" || res == "") {
		return Pattern{Action: Key(k), Scope: Scope{All: true}}, nil
	}
	if res == "" && !strings.HasPrefix(string(action), PluginPrefix) {
		return Pattern{}, ErrInvalidKey
	}
	s, err := parseScope(res, action == AccountCreate, last)
	if err != nil {
		return Pattern{}, err
	}
	return Pattern{Action: action, Scope: s}, nil
}

// Canonical is the canonical spelling of k: trailing "/*" dropped, a scope
// of everything left out.
func Canonical(k string) (Key, error) {
	p, err := Parse(k)
	if err != nil {
		return "", err
	}
	return p.Key(), nil
}

func parseScope(res Resource, create bool, raw string) (Scope, error) {
	switch {
	case raw == "*":
		return Scope{All: true}, nil
	case raw == "team" && !create: // a new row is always the creator's own
		return Scope{Team: true}, nil
	case raw == "own" && res != ResourceUser: // a user is owned by another
		return Scope{Own: true}, nil
	case res == ResourceUser || raw == "team" || raw == "own":
		return Scope{}, ErrInvalidKey
	}
	if res == "" { // a plugin action: "<resource>/<value>", nothing else
		r, val, _ := strings.Cut(raw, "/")
		if !scopeResourceRe.MatchString(r) || !ValidScopeValue(val) {
			return Scope{}, ErrInvalidKey
		}
		return Scope{Res: r, Value: val}, nil
	}
	parts := trimWildcards(res, strings.Split(raw, "/"))
	if len(parts)%2 != 0 {
		return Scope{}, ErrInvalidKey
	}
	var s Scope
	var err error
	switch {
	case len(parts) == 0:
		return Scope{All: true}, nil
	case res == ResourceProxy:
		if len(parts) != 2 || parts[0] != "proxy" {
			return Scope{}, ErrInvalidKey
		}
		s.ID, err = id(parts[1], false)
	case parts[0] == "account" && len(parts) == 2:
		s.ID, err = id(parts[1], false)
	case parts[0] == "group" && (len(parts) == 2 || len(parts) == 4 && parts[2] == "account"):
		if s.Group, err = id(parts[1], true); err == nil && len(parts) == 4 {
			s.ID, err = id(parts[3], false)
		}
	default:
		return Scope{}, ErrInvalidKey
	}
	if err != nil || create && s.ID != 0 {
		// An id range means nothing for creating a row.
		return Scope{}, ErrInvalidKey
	}
	return s, nil
}

// trimWildcards drops the trailing "/*" of a scope path of res: a lone
// "*" ("group/12/*") and the row type of res with "*" ("group/12/account/*",
// "account/*"), but not a group's "*" ("group/*" is any group, not
// everything) and nothing else ("foo/*" stays, and fails to parse).
func trimWildcards(res Resource, parts []string) []string {
	for n := len(parts); n > 0 && parts[n-1] == "*"; n = len(parts) {
		switch {
		case n%2 == 1:
			parts = parts[:n-1]
		case parts[n-2] == string(res):
			parts = parts[:n-2]
		default:
			return parts
		}
	}
	return parts
}

// id parses a positive decimal id without leading zeros, or "*" when
// wildcard is allowed.
func id(raw string, wildcard bool) (int64, error) {
	if wildcard && raw == "*" {
		return Wildcard, nil
	}
	if raw == "" || raw[0] == '0' || raw[0] == '+' {
		return 0, ErrInvalidKey
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, ErrInvalidKey
	}
	return n, nil
}

// Suffix is the canonical scope text of p, "" for everything (a p row's
// scope in casbin_rule, AllRows for "").
func (p Pattern) Suffix() string {
	s := p.Scope
	switch {
	case s.All:
		return ""
	case s.Own:
		return "own"
	case s.Team:
		return "team"
	case s.Res != "":
		return s.Res + "/" + s.Value
	}
	seg := func(n int64) string {
		if n == Wildcard {
			return "*"
		}
		return strconv.FormatInt(n, 10)
	}
	var b strings.Builder
	if s.Group != 0 {
		b.WriteString("group/" + seg(s.Group))
		if s.ID == 0 {
			return b.String()
		}
		b.WriteString("/")
	}
	b.WriteString(string(scopable[p.Action]) + "/" + seg(s.ID))
	return b.String()
}

// Key is the canonical key of p.
func (p Pattern) Key() Key {
	if suffix := p.Suffix(); suffix != "" {
		return p.Action + ":" + Key(suffix)
	}
	return p.Action
}

// Implies reports whether holding have grants want: the same action, and a
// scope reaching at least what want reaches, judged by the text alone, never
// by where rows are now (group/12 does not imply account/125 even while
// account 125 is in group 12; team implies own, but no group or id).
func Implies(have, want Pattern) bool {
	h, w := have.Scope, want.Scope
	switch {
	case have.Action != want.Action:
		return false
	case h.All:
		return true
	case w.All:
		return false
	case h.Team:
		return w.Team || w.Own
	case w.Team:
		return false
	case h.Own || w.Own:
		return h.Own && w.Own
	case h.Group == 0: // account/X, proxy/X or a plugin <res>/<value>: that row, with or without a group
		return h.ID == w.ID && h.Res == w.Res && h.Value == w.Value
	case w.Group == 0: // a group range never covers an ungrouped id
		return false
	case h.Group != Wildcard && h.Group != w.Group:
		return false
	default: // the group matches; an id in have must be want's
		return h.ID == 0 || h.ID == w.ID
	}
}

// ImpliesAny reports whether one of have implies want.
func ImpliesAny(have []Pattern, want Pattern) bool {
	for _, h := range have {
		if Implies(h, want) {
			return true
		}
	}
	return false
}

// AccountAction is the account action key of action ("read" → account:read).
func AccountAction(action string) Key { return "account:" + Key(action) }

// CanonicalSet is keys canonical, sorted, without duplicates; a key that
// does not parse is dropped.
func CanonicalSet(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if c, err := Canonical(k); err == nil {
			out = append(out, string(c))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
