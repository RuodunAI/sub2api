package authz

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The built-in roles of a new database are pinned (CONTRACTS §4, §102.1):
// the permissions SyncCore stores for each, its assignable list and the
// core menus a holder of it alone sees. A difference is a change of what
// users may do: make it on purpose, then UPDATE_GOLDEN=1 go test
// ./internal/authz -run TestBuiltinRolesGolden rewrites the file, and the
// role tables of CONTRACTS follow.
func TestBuiltinRolesGolden(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	roles, err := s.ListRoles(ctx)
	require.NoError(t, err)
	var b strings.Builder
	for _, def := range builtinRoles {
		i := slices.IndexFunc(roles, func(r *Role) bool { return r.Key == def.key })
		require.GreaterOrEqual(t, i, 0, def.key)
		r := roles[i]
		fmt.Fprintf(&b, "%s rev=%d superuser=%t assign_custom=%t\n", r.Key, def.rev, r.Superuser, r.AssignCustom)
		fmt.Fprintf(&b, "  assignable: %s\n", cmp.Or(strings.Join(r.AssignableRoleKeys, " "), "-"))
		for _, k := range slices.Sorted(slices.Values(r.PermissionKeys)) {
			fmt.Fprintf(&b, "  %s\n", k)
		}
		menus, err := s.Menus(ctx, addUser(t, s, def.key+"@golden.test", def.key))
		require.NoError(t, err)
		for _, m := range menus {
			fmt.Fprintf(&b, "  menu %s: %s\n", m.Section, menuPaths(m))
		}
	}
	path := filepath.Join("testdata", "builtin_roles.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), b.String(), "built-in roles changed: see the test's comment")
}

// What a supplier agent may not do to its team (D4, D5), through the
// authorizer: its team's accounts are read and their usage too, nothing
// else of them reaches past its own, its team's proxies not at all; its
// team's users are read, created and edited, never deleted or reset, and
// nothing of money, user groups, groups, roles' definitions or account
// owners is its.
func TestSupplierAgentStopsAtItsTeam(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	sub := chain(t, s, agent, 1, RoleUser, RoleSupplier)[0]
	ctx := core.WithUserID(context.Background(), agent)

	team := []int64{agent, sub}
	for action, want := range map[perm.Key][]int64{
		perm.AccountRead: team, perm.UsageAccountRead: team,
		perm.AccountCreate: {agent}, perm.AccountUpdate: {agent}, perm.AccountDelete: {agent},
		perm.AccountTest: {agent}, perm.AccountCredentialView: {agent},
		perm.ProxyRead: {agent}, perm.ProxyManage: {agent},
	} {
		scope, err := core.Scope(ctx, s, action)
		require.NoError(t, err, action)
		require.False(t, scope.All, action)
		require.Equal(t, want, scope.Owners, action)
	}
	users, err := core.Scope(ctx, s, perm.UserUpdate)
	require.NoError(t, err)
	require.False(t, users.All)
	for _, key := range []perm.Key{
		perm.UserRead, perm.UserCreate, perm.UserUpdate, perm.UserDelete, perm.UserPasswordReset,
		perm.BalanceAllRead, perm.BalanceAdjust, perm.SegmentRead, perm.SegmentManage, perm.GroupRead, perm.GroupManage,
		perm.AccountGroupBind, perm.AccountRelay, perm.AccountSettingsCustom, perm.AccountOwnerUpdate,
		perm.RoleDefine, perm.UsageAllRead,
	} {
		mustCan(t, s, agent, string(key), false)
	}
	for _, key := range []string{"user:read:team", "user:update:team", "role:assign", "role:read"} {
		mustCan(t, s, agent, key, true)
	}
}
