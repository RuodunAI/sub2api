package casbinx

import (
	"fmt"
	"slices"

	"github.com/casbin/casbin/v3"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// snapshot is one immutable enforcer, built from one consistent read of the
// policy. Nothing mutates it after build, so concurrent grants and enforce
// need no lock; a newer version replaces it whole (snapshots).
type snapshot struct {
	version int64
	e       *casbin.Enforcer
}

// build makes the snapshot of policy. It loads the p rows of active actions
// and every SuperKey row, plus the self permissions (self, the keys of
// authz.SelfPermissionKeys: this package cannot import authz) as
// SelfRole's p rows, and no g row: a user's roles come with each call.
func build(policy Policy, self []string) (*snapshot, error) {
	ps := make([][]string, 0, len(policy.Rules)+len(self))
	for _, r := range policy.Rules {
		if loadable(r, policy.Active) {
			ps = append(ps, []string{r.V0, r.V1, r.V2})
		}
	}
	for _, k := range self {
		p, err := perm.Parse(k)
		if err != nil {
			return nil, fmt.Errorf("casbinx: self permission %q: %w", k, err)
		}
		ps = append(ps, []string{SelfRole, string(p.Action), scopeOf(p)})
	}
	e, err := newEnforcer()
	if err != nil {
		return nil, err
	}
	if len(ps) > 0 {
		if err := e.GetModel().AddPolicies("p", "p", ps); err != nil {
			return nil, err
		}
	}
	// Binds the role manager to the model's g.
	if err := e.BuildRoleLinks(); err != nil {
		return nil, err
	}
	return &snapshot{version: policy.Version, e: e}, nil
}

// loadable reports whether r is a p row the snapshot holds: a superuser row,
// or a grant of an active action. Grants of disabled and removed actions
// stay stored and come back with their action. Not loaded:
//   - a row with an empty column: an empty scope must not read as every
//     row;
//   - a stored row of SelfRole (a role keyed "_self", which only hand
//     written SQL can make): every active user holds SelfRole, so its
//     grants would reach everyone, where the legacy loader gives them to
//     the role's holders only.
func loadable(r Rule, active map[string]bool) bool {
	if r.Ptype != "p" || r.V0 == "" || r.V1 == "" || r.V2 == "" || r.V0 == SelfRole {
		return false
	}
	return r.V1 == SuperKey || active[r.V1]
}

// scopeOf is p's scope column: its suffix, AllRows for every row.
func scopeOf(p perm.Pattern) string {
	if s := p.Suffix(); s != "" {
		return s
	}
	return AllRows
}

// subjects are the policy subjects whose grants ur holds: its roles and
// SelfRole when active, none when not.
func subjects(ur UserRoles) []string {
	if !ur.Active {
		return nil
	}
	return append(slices.Clone(ur.Roles), SelfRole)
}

// grants is the effective permission set of a user holding ur (read at the
// snapshot's version, which labels the set): the p rows of its subjects; a
// SuperKey row sets Superuser.
func (s *snapshot) grants(ur UserRoles) (core.PermissionSet, error) {
	set := core.PermissionSet{Keys: map[string]struct{}{}, Version: s.version}
	for _, sub := range subjects(ur) {
		rows, err := s.e.GetPermissionsForUser(sub)
		if err != nil {
			return core.PermissionSet{}, err
		}
		for _, r := range rows {
			if r[1] == SuperKey {
				set.Superuser = true
				continue
			}
			set.Add(grantKey(r[1], r[2]))
		}
	}
	return set, nil
}

// enforce runs key through the matcher for a user holding ur: allowed when
// one of its subjects is (some(where p.eft == allow) over the union).
func (s *snapshot) enforce(ur UserRoles, key string) (bool, error) {
	for _, sub := range subjects(ur) {
		if ok, err := s.e.Enforce(sub, key); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}
