package authz

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Scoped grants (CONTRACTS §21): a role write stores a grant of every row
// as scope * and a scoped one with its scope (casbin_rule), lists them
// canonical and refuses keys that do not parse or name no permission (the
// former own-level spelling among them); holders get exactly those scopes.
func TestScopedRoleGrants(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	rows := func(id int64) (plain, scoped []string) { return grantRows(t, s, id) }

	scopeRows(t, db, "account:update:group/12", "proxy:manage:proxy/8")
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "group_ops", Name: map[string]string{"en": "Group ops"},
		PermissionKeys: []string{"account:update:group/12/*", "account:read:own", "proxy:manage:proxy/8", "user:read", "account:test:*"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"user:read", "account:read:own", "account:update:group/12", "account:test", "proxy:manage:proxy/8"}, r.PermissionKeys)
	plain, scoped := rows(r.ID)
	require.Equal(t, []string{"account:test", "user:read"}, plain)
	require.Equal(t, []string{"account:read:own", "account:update:group/12", "proxy:manage:proxy/8"}, scoped)

	holder := addUser(t, s, "ops@x.com", "group_ops")
	set, err := s.PermissionSet(ctx, holder)
	require.NoError(t, err)
	require.False(t, set.Has("account:read"))
	require.False(t, set.Has("account:update"))
	require.True(t, set.Has("account:update:group/12/account/5"))
	require.True(t, set.Has("account:read:own"))

	// Narrowing drops the scope; a redundant own next to the full key is
	// not kept.
	r, err = s.SetRolePermissions(ctx, root, r.ID, []string{"account:update:group/12", "account:test", "account:test:own"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"account:test", "account:update:group/12"}, slices.Sorted(slices.Values(r.PermissionKeys)))
	plain, scoped = rows(r.ID)
	require.Equal(t, []string{"account:test"}, plain)
	require.Equal(t, []string{"account:update:group/12"}, scoped)

	// Scoped grants only: every unscoped grant goes (a NULL id list once
	// kept them); then nothing at all.
	r, err = s.SetRolePermissions(ctx, root, r.ID, []string{"account:read:own"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"account:read:own"}, r.PermissionKeys)
	plain, scoped = rows(r.ID)
	require.Empty(t, plain)
	require.Equal(t, []string{"account:read:own"}, scoped)
	set, err = s.PermissionSet(ctx, holder)
	require.NoError(t, err)
	require.False(t, set.Has("account:test"))
	require.True(t, set.Has("account:read:own"))

	r, err = s.SetRolePermissions(ctx, root, r.ID, []string{}, nil)
	require.NoError(t, err)
	plain, scoped = rows(r.ID)
	require.Empty(t, plain)
	require.Empty(t, scoped)

	for _, bad := range []string{"user:read:own", "account:create:account/5", "account:read:proxy/1", "account:read:group/01", "account:own:read"} {
		_, err = s.SetRolePermissions(ctx, root, r.ID, []string{bad}, nil)
		var ce *core.Error
		require.True(t, errors.As(err, &ce) && ce.Code == "invalid_argument", "%s: %v", bad, err)
	}

	// The editor lists actions with their scope type.
	mods, err := s.ListPermissions(ctx)
	require.NoError(t, err)
	types := map[string]string{}
	for _, m := range mods {
		for _, p := range m.Permissions {
			types[p.Key] = p.ScopeType
		}
	}
	require.Equal(t, "account", types["account:credential:view"])
	require.Equal(t, "proxy", types["proxy:read"])
	require.Equal(t, "", types["account:relay"])
}

// A grant may only name rows that exist (C3): a deleted account, a group
// or proxy never created, is refused, on a new role and on a role write; a
// grant the role holds already stays. ClearScopeGrants announces the
// version itself (S2): another node drops the holders' sets without the
// deleting module saying anything.
func TestScopeGrantsNameExistingRows(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	bus := newMemBus()
	s := newService(t, db, bus, nil)
	other := newService(t, db, bus, nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	scopeRows(t, db, "account:read:group/12/account/125", "proxy:read:proxy/8")

	for _, keys := range [][]string{{"account:read:group/77"}, {"account:read:account/999"}, {"account:read:group/12/account/126"}, {"proxy:read:proxy/9"}} {
		_, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "nope", Name: lt("n", "n"), PermissionKeys: keys})
		require.True(t, isCode(err, "invalid_argument"), "%v: %v", keys, err)
		require.Equal(t, "unknown_scope", fieldCode(err, "permission_keys"), "%v", keys)
	}
	keys := []string{"account:read:group/12/account/125", "proxy:read:proxy/8"}
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "scoped", Name: lt("s", "s"), PermissionKeys: keys})
	require.NoError(t, err)
	holder := addUser(t, s, "h@x.com", "scoped")
	set, err := other.PermissionSet(ctx, holder)
	require.NoError(t, err)
	require.True(t, set.Has("proxy:read:proxy/8"))

	// The proxy goes (as proxy's delete does): its grant is cleared, and
	// the other node hears of it from authz.
	require.NoError(t, s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM proxies WHERE id = 8`).Error; err != nil {
			return err
		}
		v, err := s.ClearScopeGrants(ctx, tx, root, core.ScopeRefProxy, []int64{8})
		require.NotZero(t, v)
		return err
	}))
	eventually(t, func() bool {
		set, err := other.PermissionSet(ctx, holder)
		return err == nil && !set.Has("proxy:read:proxy/8")
	})
	// Saving the role back with the cleared grant listed: refused; the
	// grants it holds stay.
	_, err = s.SetRolePermissions(ctx, root, r.ID, keys, nil)
	require.Equal(t, "unknown_scope", fieldCode(err, "permission_keys"), "%v", err)
	_, err = s.SetRolePermissions(ctx, root, r.ID, keys[:1], nil)
	require.NoError(t, err)
}
