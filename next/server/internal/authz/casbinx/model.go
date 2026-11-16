package casbinx

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// modelText is model.conf (CONTRACTS §99). A p row is one grant: p.act a
// catalog action, p.scope its canonical scope (AllRows for every row, never
// empty); a superuser role holds act SuperKey. r.obj is the key asked for,
// in any spelling: permMatch parses it. No domains: a scope is a resource
// path, not a tenant, and RBAC with domains would multiply g by scopes.
const modelText = `
[request_definition]
r = sub, obj

[policy_definition]
p = sub, act, scope

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && permMatch(p.act, p.scope, r.obj)
`

// permMatchName is the matcher function. Like go-admin's keyMatch2Cached it
// must not reuse a Casbin builtin's name: AddFunction is a LoadOrStore, so a
// clash would be ignored silently and the builtin would judge.
const permMatchName = "permMatch"

// grantKey is the permission key of the grant (act, scope): the action alone
// for every row, else "<act>:<scope>".
func grantKey(act, scope string) string {
	if scope == AllRows {
		return act
	}
	return act + ":" + scope
}

// matcher is permMatch with its perm.Parse cache: one per enforcer
// (newEnforcer), so that no parse outlives the enforcer that needed it and
// the package holds no state of its own.
type matcher struct {
	cache sync.Map // key -> parseResult
	size  atomic.Int64
}

// permMatch reports whether the grant (act, scope) implies the key want:
// perm.Implies on the parsed keys, the function PermissionSet.Has uses, so
// the matcher and the hot path cannot judge apart. act SuperKey implies
// every key, future ones included; a key that does not parse matches
// nothing.
func (m *matcher) permMatch(act, scope, want string) bool {
	if act == SuperKey {
		return true
	}
	have, ok := m.parsed(grantKey(act, scope))
	if !ok {
		return false
	}
	w, ok := m.parsed(want)
	return ok && perm.Implies(have, w)
}

var errPermMatchArgs = errors.New("casbinx: permMatch takes 3 string arguments")

// fn is permMatch as the function the model's matcher calls.
func (m *matcher) fn(args ...any) (any, error) {
	if len(args) != 3 {
		return false, errPermMatchArgs
	}
	act, ok1 := args[0].(string)
	scope, ok2 := args[1].(string)
	want, ok3 := args[2].(string)
	if !ok1 || !ok2 || !ok3 {
		return false, errPermMatchArgs
	}
	return m.permMatch(act, scope, want), nil
}

// parseCacheLimit bounds a matcher's perm.Parse cache. Keys come from code
// constants, the catalog and the grants, a bounded set; past the limit a key
// is still parsed, just not kept (go-admin's keyMatch2Cached does the same
// for its compiled route patterns).
const parseCacheLimit = 8192

type parseResult struct {
	p  perm.Pattern
	ok bool
}

// parsed is perm.Parse, cached: Enforce runs the matcher on every p row, so
// each call would otherwise parse the same few hundred keys again.
func (m *matcher) parsed(k string) (perm.Pattern, bool) {
	if v, ok := m.cache.Load(k); ok {
		r := v.(parseResult)
		return r.p, r.ok
	}
	p, err := perm.Parse(k)
	r := parseResult{p, err == nil}
	if m.size.Load() < parseCacheLimit {
		if _, loaded := m.cache.LoadOrStore(k, r); !loaded {
			m.size.Add(1)
		}
	}
	return r.p, r.ok
}

// newModel parses modelText.
func newModel() (model.Model, error) { return model.NewModelFromString(modelText) }

// newEnforcer is the one constructor of an enforcer (go-admin's
// newEnforcer): every enforcer gets permMatch before its first Enforce
// compiles the matcher, has the read-only role manager, and is a read
// replica.
//   - AutoSave off: it has no adapter, and nothing may write through it.
//   - The g function cache off: Casbin keeps the g memo with the compiled
//     matcher for the enforcer's lifetime. g only links a role subject to
//     itself today (the users' roles come with each call), so the memo
//     could not serve a stale link; off all the same, so that no role
//     manager answering from changing data is ever memoized.
//   - No logger is set, so Casbin logs nothing.
func newEnforcer() (*casbin.Enforcer, error) {
	m, err := newModel()
	if err != nil {
		return nil, err
	}
	e, err := casbin.NewEnforcer(m)
	if err != nil {
		return nil, err
	}
	e.EnableAutoSave(false)
	e.AddFunction(permMatchName, new(matcher).fn)
	e.SetRoleManager(roleManager{})
	e.EnableGFunctionCache(false)
	return e, nil
}
