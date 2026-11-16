package authz

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// orphans lists the casbin_rule rows naming a user, role or permission that
// does not exist (casbinrule.OrphansSQL).
func orphans(t *testing.T, s *Service) []string {
	t.Helper()
	out := []string{}
	require.NoError(t, s.db.Gorm.Raw(casbinrule.OrphansSQL).Scan(&out).Error)
	return out
}

// No foreign key reaches casbin_rule (a row names its role and permission
// by key): each delete path clears the rows of what it deletes, in its
// transaction, so none is left behind and a key created again starts with
// nothing. testutil.DB looks for orphans after every test besides.
func TestDeletePathsClearPolicyRows(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	tx := func(fn func(tx *gorm.DB) error) { t.Helper(); require.NoError(t, db.Tx(ctx, fn)) }

	// A role: its grants and its holders' assignments.
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "temp", Name: map[string]string{"en": "Temp"},
		PermissionKeys: []string{"user:read", "account:read:own"}})
	require.NoError(t, err)
	holder := addUser(t, s, "holder@x.com", "temp", RoleUser)
	require.NoError(t, s.DeleteRole(ctx, root, r.ID))
	require.Empty(t, testutil.GrantKeys(t, db, "temp"))
	require.Equal(t, []string{RoleUser}, testutil.RoleKeys(t, db, holder))
	require.Empty(t, orphans(t, s), "after DeleteRole")
	r, err = s.CreateRole(ctx, root, CreateRoleInput{Key: "temp", Name: map[string]string{"en": "Temp"}})
	require.NoError(t, err)
	require.Empty(t, r.PermissionKeys, "a role created again with the key")

	// A plugin's permissions: granted for every row and scoped, then
	// uninstalled; its staged ones discarded.
	sync := syncer(t, s, "gone")
	flow := map[string][]string{RoleAdmin: {""}, RoleOperator: {"own"}}
	scope := &manifest.PermissionScope{Resource: "flow", Own: true, Options: "/scopes/flow"}
	require.NoError(t, sync(core.PermissionDef{Key: "flow:read", ScopeType: "plugin:flow", Scope: scope, GrantRoles: flow},
		core.PermissionDef{Key: "flow:next", Staged: true}))
	tx(func(tx *gorm.DB) error {
		return s.GrantPluginPermission(ctx, tx, "plugin.gone:flow:next", map[string][]string{RoleUser: {""}})
	})
	require.Contains(t, testutil.GrantKeys(t, db, RoleUser), "plugin.gone:flow:next")
	tx(func(tx *gorm.DB) error { return s.DiscardStaged(ctx, tx, "gone") })
	require.NotContains(t, testutil.GrantKeys(t, db, RoleUser), "plugin.gone:flow:next")
	require.Empty(t, orphans(t, s), "after DiscardStaged")
	require.Contains(t, testutil.GrantKeys(t, db, RoleOperator), "plugin.gone:flow:read:own")
	tx(func(tx *gorm.DB) error {
		if err := s.DeletePlugin(ctx, tx, "gone"); err != nil {
			return err
		}
		return tx.Exec(`DELETE FROM plugins WHERE key = 'gone'`).Error
	})
	for _, role := range []string{RoleAdmin, RoleOperator} {
		require.False(t, slices.ContainsFunc(testutil.GrantKeys(t, db, role), func(k string) bool { return strings.HasPrefix(k, "plugin.gone:") }), role)
	}
	require.Empty(t, orphans(t, s), "after DeletePlugin")

	// Scoped grants naming a deleted group (core.ClearScopeGrants).
	group := "group/" + strconv.FormatInt(testutil.InsertGroup(t, db, "doomed"), 10)
	testutil.Grant(t, db, RoleOperator, "account:read", group, group+"/account/5")
	tx(func(tx *gorm.DB) error {
		id, _ := strconv.ParseInt(strings.TrimPrefix(group, "group/"), 10, 64)
		_, err := core.ClearScopeGrants(ctx, tx, root, core.ScopeRefGroup, []int64{id})
		return err
	})
	require.False(t, slices.ContainsFunc(testutil.GrantKeys(t, db, RoleOperator), func(k string) bool { return strings.Contains(k, ":"+group) }))
	require.Empty(t, orphans(t, s), "after ClearScopeGrants")
}
