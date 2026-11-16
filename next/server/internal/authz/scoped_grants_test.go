package authz

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Granting and managing compare scopes by implication, from the text of the
// keys alone (CONTRACTS §21.0, R6): who manages group 12 grants group 12 and
// what it implies, never group 13 nor an account id outside the group
// syntax, and acts on holders of a subset of its range only; own is
// relative (an own holder grants own). Denials name the canonical keys.
func TestScopedGrantsAndManagement(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	role := func(key string, perms ...string) {
		t.Helper()
		scopeRows(t, db, perms...)
		_, err := s.CreateRole(ctx, root, CreateRoleInput{Key: key, Name: lt(key, key), PermissionKeys: perms})
		require.NoError(t, err)
	}
	manage := []string{"role:define", "role:assign", "user:update"}
	role("ops_a", append(manage, "account:read:group/12", "account:update:group/12")...)
	role("sub_a", "account:update:group/12/account/125", "account:read:group/12/*")
	role("ops_b", "account:update:group/13")
	role("own_ops", append(manage, "account:update:own")...)
	role("own_sub", "account:update:own")
	role("star", "account:update:group/*")
	letAssign(t, s, "ops_a", "sub_a", "ops_b")
	letAssign(t, s, "own_ops", "own_sub")
	a := addUser(t, s, "a@x.com", "ops_a")
	own := addUser(t, s, "own@x.com", "own_ops")
	star := addUser(t, s, "star@x.com", "star")

	for _, tc := range []struct {
		actor   int64
		keys    []string
		missing []string // nil: granted
	}{
		{a, []string{"account:update:group/12/account/125", "account:read:group/12"}, nil},
		{a, []string{"account:update:group/12/*"}, nil},
		{a, []string{"account:update:account/125"}, []string{"account:update:account/125"}},
		{a, []string{"account:update:group/13", "account:update:group/12"}, []string{"account:update:group/13"}},
		{a, []string{"account:update"}, []string{"account:update"}},
		{a, []string{"account:update:group/*"}, []string{"account:update:group/*"}},
		{a, []string{"account:update:own"}, []string{"account:update:own"}},
		{a, []string{"account:delete:group/12"}, []string{"account:delete:group/12"}},
		{star, []string{"account:update:group/12", "account:update:group/*/account/5"}, nil},
		{star, []string{"account:update:account/5"}, []string{"account:update:account/5"}},
		{own, []string{"account:update:own"}, nil},
		{own, []string{"account:update:group/1"}, []string{"account:update:group/1"}},
	} {
		err := s.CanGrant(ctx, tc.actor, tc.keys)
		if tc.missing == nil {
			require.NoError(t, err, "%v", tc.keys)
			continue
		}
		var ce *core.Error
		require.True(t, errors.As(err, &ce) && ce.Code == "permission_denied", "%v: %v", tc.keys, err)
		require.Equal(t, tc.missing, ce.Details["missing"], "%v", tc.keys)
	}

	// Audits name the canonical keys, whatever spelling was sent.
	spelled, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "spelled", Name: lt("s", "s"), PermissionKeys: []string{"account:read:own", "account:update:group/12/account/*"}})
	require.NoError(t, err)
	var detail string
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT detail->>'permission_keys' FROM audit_logs WHERE action = 'role.create' AND target_id = $1`, fmt.Sprint(spelled.ID)).Scan(&detail))
	require.JSONEq(t, `["account:read:own", "account:update:group/12"]`, detail)

	// Defining a role: the same rule.
	scopeRows(t, db, "account:read:group/12/account/7")
	_, err = s.CreateRole(ctx, a, CreateRoleInput{Key: "a_child", Name: lt("c", "c"), PermissionKeys: []string{"account:read:group/12/account/7"}})
	require.NoError(t, err)
	_, err = s.CreateRole(ctx, a, CreateRoleInput{Key: "a_wide", Name: lt("w", "w"), PermissionKeys: []string{"account:read:group/13"}})
	require.True(t, isCode(err, "permission_denied"), "%v", err)

	// Assigning and acting on users: a subset of A's range, not B's.
	x, y := addUser(t, s, "x@x.com"), addUser(t, s, "y@x.com")
	require.NoError(t, setRoles(s, a, x, "sub_a"))
	err = setRoles(s, a, y, "ops_b")
	require.True(t, isCode(err, "permission_denied"), "%v", err)
	b := addUser(t, s, "b@x.com", "ops_b")
	require.True(t, isCode(s.CanActOnUsers(ctx, a, core.ActOnIdentity, b), "permission_denied"))
	require.NoError(t, s.CanActOnUsers(ctx, a, core.ActOnIdentity, x))

	// own is relative: an own holder hands own to others and manages them.
	z := addUser(t, s, "z@x.com")
	require.NoError(t, setRoles(s, own, z, "own_sub"))
	require.NoError(t, s.CanActOnUsers(ctx, own, core.ActOnIdentity, z))
	// Not a group manager, though.
	require.True(t, isCode(s.CanActOnUsers(ctx, own, core.ActOnIdentity, x), "permission_denied"))
}

// A plugin permission's scopes are checked against its catalog entry when a
// role is written: a scope of its resource (of its values for an enum) and
// own where allowed are saved, anything else is invalid, a wildcard is no
// key (D1). Covering compares plugin scopes like core ones: who holds every
// row grants a scope, never the other way.
func TestPluginScopedKeysResolveAndCover(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	sync := syncer(t, s, "m")
	enum := &manifest.PermissionScope{Resource: manifest.ScopeValueResource, Values: []manifest.ScopeValue{{Key: "input"}}}
	require.NoError(t, sync(
		core.PermissionDef{Key: "flow:manage", ScopeType: "plugin:flow", Scope: &manifest.PermissionScope{Resource: "flow", Own: true}},
		core.PermissionDef{Key: "stage:run", ScopeType: "plugin:value", Scope: enum},
		core.PermissionDef{Key: "plain:read"}))
	create := func(actor int64, key string, perms ...string) (*Role, error) {
		return s.CreateRole(ctx, actor, CreateRoleInput{Key: key, Name: lt(key, key), PermissionKeys: perms})
	}

	r, err := create(root, "scoped", "plugin.m:flow:manage:flow/3", "plugin.m:flow:manage:own", "plugin.m:stage:run:value/input")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"plugin.m:flow:manage:flow/3", "plugin.m:flow:manage:own", "plugin.m:stage:run:value/input"}, r.PermissionKeys)
	for i, key := range []string{"plugin.m:plain:read:own", "plugin.m:flow:manage:ticket/3", "plugin.m:stage:run:value/output", "plugin.m:stage:run:own"} {
		_, err := create(root, fmt.Sprintf("bad_%d", i), key)
		require.Equal(t, "invalid", fieldCode(err, "permission_keys"), "%s: %v", key, err)
	}
	_, err = create(root, "wild", "plugin.m:flow:*")
	require.Equal(t, "unknown", fieldCode(err, "permission_keys"), "%v", err)

	// Every row makes the scopes redundant.
	r, err = create(root, "full", "plugin.m:flow:manage", "plugin.m:flow:manage:flow/3")
	require.NoError(t, err)
	require.Equal(t, []string{"plugin.m:flow:manage"}, r.PermissionKeys)

	_, err = create(root, "lead_full", "role:define", "plugin.m:flow:manage")
	require.NoError(t, err)
	_, err = create(root, "lead_one", "role:define", "plugin.m:flow:manage:flow/3")
	require.NoError(t, err)
	full, one := addUser(t, s, "full@x.com", "lead_full"), addUser(t, s, "one@x.com", "lead_one")
	_, err = create(full, "child_one", "plugin.m:flow:manage:flow/3")
	require.NoError(t, err)
	_, err = create(one, "child_full", "plugin.m:flow:manage")
	var ce *core.Error
	require.True(t, errors.As(err, &ce) && ce.Code == "permission_denied", "%v", err)
	require.Equal(t, []string{"plugin.m:flow:manage"}, ce.Details["missing"])
}

// team is a scope like own, one level wider (CONTRACTS §103.2): a role
// saves it for the actions that take it (users: every user or team only),
// its holder grants team and own but not every row, and an own holder does
// not grant team.
func TestTeamScopedGrants(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	create := func(actor int64, key string, perms ...string) (*Role, error) {
		return s.CreateRole(ctx, actor, CreateRoleInput{Key: key, Name: lt(key, key), PermissionKeys: perms})
	}
	r, err := create(root, "lead", "role:define", "account:read:team", "account:read:own", "user:read:team", "usage:account:read:team")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"role:define", "account:read:team", "user:read:team", "usage:account:read:team"}, r.PermissionKeys, "own folds into team")
	for i, key := range []string{"user:read:own", "user:update:account/3", "account:create:team"} {
		_, err := create(root, fmt.Sprintf("bad_%d", i), key)
		require.Equal(t, "invalid", fieldCode(err, "permission_keys"), "%s: %v", key, err)
	}
	_, err = create(root, "own_lead", "role:define", "account:read:own")
	require.NoError(t, err)

	lead, ownLead := addUser(t, s, "lead@x.com", "lead"), addUser(t, s, "own@x.com", "own_lead")
	require.NoError(t, s.CanGrant(ctx, lead, []string{"account:read:own", "account:read:team", "user:read:team", "usage:account:read:own"}))
	for _, tc := range []struct {
		actor int64
		key   string
	}{{lead, "account:read"}, {lead, "user:read"}, {lead, "usage:account:read:group/1"}, {ownLead, "account:read:team"}} {
		err := s.CanGrant(ctx, tc.actor, []string{tc.key})
		require.Equal(t, []string{tc.key}, detail(err, "missing"), "%d %s: %v", tc.actor, tc.key, err)
	}
}
