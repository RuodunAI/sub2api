package casbinx

import (
	"errors"
	"fmt"

	"github.com/casbin/casbin/v3"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// Options of Build.
type Options struct {
	// Roles answers g: the users' roles (the L1 of RoleCache). Required.
	Roles RoleSource
	// Self lists the self permission keys (authz.SelfPermissionKeys), held
	// by SelfRole. They are not in the catalog, so authz hands them over:
	// this package cannot import authz.
	Self []string
}

// Snapshot is one immutable enforcer, built from one consistent read of the
// policy. Nothing mutates it after Build, so concurrent Grants and Enforce
// need no lock; a newer version replaces it whole (Engine).
type Snapshot struct {
	version int64
	roles   RoleSource
	e       *casbin.Enforcer
}

var errNoRoleSource = errors.New("casbinx: Build needs a RoleSource")

// Build makes the snapshot of policy. It loads the p rows of active actions
// and every SuperKey row, plus the self permissions as SelfRole's p rows,
// and no g row: users' roles come from o.Roles.
func Build(policy Policy, o Options) (*Snapshot, error) {
	if o.Roles == nil {
		return nil, errNoRoleSource
	}
	ps := make([][]string, 0, len(policy.Rules)+len(o.Self))
	for _, r := range policy.Rules {
		if loadable(r, policy.Active) {
			ps = append(ps, []string{r.V0, r.V1, r.V2})
		}
	}
	for _, k := range o.Self {
		p, err := perm.Parse(k)
		if err != nil {
			return nil, fmt.Errorf("casbinx: self permission %q: %w", k, err)
		}
		ps = append(ps, []string{SelfRole, string(p.Action), scopeOf(p)})
	}
	e, err := newEnforcer(o.Roles)
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
	return &Snapshot{version: policy.Version, roles: o.Roles, e: e}, nil
}

// loadable reports whether r is a p row the snapshot holds: a superuser row,
// or a grant of an active action. Grants of disabled and removed actions
// stay stored and come back with their action. A row with an empty column
// is never loaded: an empty scope must not read as every row.
func loadable(r Rule, active map[string]bool) bool {
	if r.Ptype != "p" || r.V0 == "" || r.V1 == "" || r.V2 == "" {
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

// Version is the authz version the snapshot was read at.
func (s *Snapshot) Version() int64 { return s.version }

// Grants is uid's effective permission set as Casbin resolves it: the p rows
// of the roles the RoleSource gives uid, SelfRole's included; a SuperKey row
// sets Superuser. Version is the snapshot's. ErrNotWarm when the source
// holds no entry for uid.
func (s *Snapshot) Grants(uid int64) (core.PermissionSet, error) {
	rows, err := s.e.GetImplicitPermissionsForUser(UserSub(uid))
	if err != nil {
		return core.PermissionSet{}, err
	}
	set := core.PermissionSet{Keys: map[string]struct{}{}, Version: s.version}
	for _, r := range rows {
		if r[1] == SuperKey {
			set.Superuser = true
			continue
		}
		set.Add(GrantKey(r[1], r[2]))
	}
	return set, nil
}

// Enforce runs key through the matcher for uid: every p row (8-230 µs at
// 400 rows), so for the shadow comparison and diagnostics only; hot paths
// use Grants' PermissionSet.Has. ErrNotWarm when the source holds no entry
// for uid (g would deny everything).
func (s *Snapshot) Enforce(uid int64, key string) (bool, error) {
	sub := UserSub(uid)
	if _, ok := s.roles.Roles(sub); !ok {
		return false, ErrNotWarm
	}
	return s.e.Enforce(sub, key)
}
