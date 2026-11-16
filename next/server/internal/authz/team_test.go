package authz

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// setOwner makes owner (0: none) the owner of user, without authz's
// checks (testutil.SetOwner).
func setOwner(t *testing.T, s *Service, user, owner int64) {
	t.Helper()
	testutil.SetOwner(t, s.db, user, owner)
}

// chained numbers the users chain adds.
var chained atomic.Int64

// chain adds n users, each owned by the one before it, the first by top
// (0: none), with roles, and returns them.
func chain(t *testing.T, s *Service, top int64, n int, roles ...string) []int64 {
	t.Helper()
	out := make([]int64, n)
	for i := range out {
		out[i] = addUser(t, s, fmt.Sprintf("c%d@x.com", chained.Add(1)), roles...)
		setOwner(t, s, out[i], top)
		top = out[i]
	}
	return out
}

// teamOf lists the users holder reaches with user:read:team, through the
// scope's statement.
func teamOf(t *testing.T, s *Service, holder int64) []int64 {
	t.Helper()
	scope := core.PermissionSetOf("user:read:team").DataScope(holder, perm.UserRead)
	var ids []int64
	require.NoError(t, s.db.Gorm.Table("users u").Scopes(core.UserRows().Scope(scope)).Order("u.id").Pluck("u.id", &ids).Error)
	return ids
}

// A team is every user below its holder, soft-deleted ones included (D7),
// without the holder, read by each statement from the owner paths: a move
// shows at once, on every node, with no cache to drop and no version to
// wait for.
func TestTeam(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	top := addUser(t, s, "top@x.com", RoleSupplierAgent)
	c := chain(t, s, top, core.MaxTeamDepth)
	side := addUser(t, s, "side@x.com", RoleSupplier)
	setOwner(t, s, side, c[0])
	_, err := db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, side)
	require.NoError(t, err)

	require.ElementsMatch(t, append([]int64{side}, c...), teamOf(t, s, top), "eight levels, the deleted user included")
	require.Equal(t, c[2:], teamOf(t, s, c[1]))
	require.Empty(t, teamOf(t, s, c[core.MaxTeamDepth-1]))

	loner := addUser(t, s, "loner@x.com", RoleSupplier)
	setOwner(t, s, loner, c[core.MaxTeamDepth-2])
	require.Contains(t, teamOf(t, s, top), loner)
	setOwner(t, s, c[1], 0)
	require.ElementsMatch(t, []int64{side, c[0]}, teamOf(t, s, top), "c[1] took its users along")
	require.Contains(t, teamOf(t, s, c[1]), loner)
}

// core.Scope (and Router.PermScoped) give a team scope its holder, as an
// owner and as the top of a team: accounts owned by the holder or a user
// below it, users below it (not itself), never anyone else's; an access
// key narrows team to own (CONTRACTS §102.2).
func TestTeamScope(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	sub := chain(t, s, agent, 2, RoleSupplier)
	other := addUser(t, s, "other@x.com", RoleSupplierAgent)
	stranger := chain(t, s, other, 1, RoleSupplier)[0]
	ctx := core.WithUserID(context.Background(), agent)
	team := core.DataScope{Owners: []int64{agent}, Teams: []int64{agent}}

	accounts, err := core.Scope(ctx, s, perm.AccountRead)
	require.NoError(t, err)
	require.Equal(t, team, accounts)
	usage, err := core.Scope(ctx, s, perm.UsageAccountRead)
	require.NoError(t, err)
	require.Equal(t, team, usage)
	update, err := core.Scope(ctx, s, perm.AccountUpdate)
	require.NoError(t, err)
	require.Equal(t, core.DataScope{Owners: []int64{agent}}, update, "own stays own")

	users, err := core.Scope(ctx, s, perm.UserRead)
	require.NoError(t, err)
	var seen []int64
	require.NoError(t, db.Gorm.Table("users u").Scopes(core.UserRows().Scope(users)).Order("u.id").Pluck("u.id", &seen).Error)
	require.Equal(t, sub, seen, "the users below, not the agent, not another agent's")
	require.NotContains(t, seen, stranger)

	keyed := core.WithAccessKey(ctx, core.AccessKeyIdentity{UserID: agent, Permissions: []string{"account:read:own", "user:read"}})
	accounts, err = core.Scope(keyed, s, perm.AccountRead)
	require.NoError(t, err)
	require.Equal(t, core.DataScope{Owners: []int64{agent}}, accounts)
	users, err = core.Scope(keyed, s, perm.UserRead)
	require.NoError(t, err)
	require.Equal(t, team, users)
}

// Whom a team holder acts on (plan 1.8, 5.2): the users below it alone,
// never itself or anyone else, and only by roles it may assign; an agent
// it makes manages a part of its team (D2). Holders of none of the user
// actions, and those holding one for every user, are not limited to a
// team.
func TestCanActOnUsersTeam(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	operator := addUser(t, s, "op@x.com", RoleOperator)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	sub := chain(t, s, agent, 1, RoleUser)[0]
	peer := chain(t, s, agent, 1, RoleUser)[0]
	outsider := addUser(t, s, "out@x.com", RoleUser)
	notInTeam := func(err error) bool {
		return isCode(err, "permission_denied") && detail(err, "reason") == "not_in_team"
	}

	require.NoError(t, s.CanActOnUsers(ctx, agent, core.ActOnIdentity, sub, peer))
	require.True(t, notInTeam(s.CanActOnUsers(ctx, agent, core.ActOnIdentity, sub, outsider)))
	require.True(t, notInTeam(s.CanActOnUsers(ctx, agent, core.ActOnIdentity, agent)), "not itself")
	require.True(t, notInTeam(setRoles(s, agent, outsider, RoleUser, RoleSupplier)), "assign outside the team")
	require.True(t, notInTeam(setRoles(s, agent, outsider, RoleUser)), "the user's own roles again: no read past the team")
	require.True(t, notInTeam(setRoles(s, agent, agent, RoleSupplierAgent, RoleSupplier)), "assign to itself")
	require.True(t, isCode(setRoles(s, agent, sub, RoleUser, RoleOperator), "permission_denied"), "not assignable")
	require.NoError(t, setRoles(s, agent, sub, RoleUser, RoleSupplier))

	// D2: the agent makes sub an agent; sub manages its own team only.
	require.NoError(t, setRoles(s, agent, sub, RoleUser, RoleSupplierAgent))
	below := chain(t, s, sub, 1, RoleUser)[0]
	require.NoError(t, s.CanActOnUsers(ctx, sub, core.ActOnIdentity, below))
	require.NoError(t, s.CanActOnUsers(ctx, agent, core.ActOnIdentity, below), "the whole subtree")
	for _, id := range []int64{agent, peer} {
		require.True(t, notInTeam(s.CanActOnUsers(ctx, sub, core.ActOnIdentity, id)), "%d", id)
	}

	// D6: the operator manages neither agents nor their making.
	require.True(t, isCode(setRoles(s, operator, outsider, RoleUser, RoleSupplierAgent), "permission_denied"))
	require.True(t, isCode(s.CanActOnUsers(ctx, operator, core.ActOnIdentity, agent), "permission_denied"))
	require.NoError(t, setRoles(s, admin, outsider, RoleUser, RoleSupplierAgent))
	require.NoError(t, s.CanActOnUsers(ctx, admin, core.ActOnIdentity, agent, sub, outsider))
	require.NoError(t, s.CanActOnUsers(ctx, operator, core.ActOnIdentity, peer), "every user: not limited")

	// A role with role:assign alone keeps acting as before.
	_, err := s.CreateRole(ctx, admin, CreateRoleInput{Key: "assigner", Name: lt("a", "a"), PermissionKeys: []string{"role:assign"}})
	require.NoError(t, err)
	letAssign(t, s, "assigner", RoleUser)
	assigner := addUser(t, s, "as@x.com", RoleUser, "assigner")
	require.NoError(t, s.CanActOnUsers(ctx, assigner, core.ActOnIdentity, addUser(t, s, "plain@x.com", RoleUser)))
}

// Deleting an account clears the usage:account:read grants naming it, as
// it clears account:read's (plan 3, ClearScopeGrants matches the scope
// whatever the action).
func TestClearScopeGrantsClearsAccountUsage(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	keys := []string{"usage:account:read:account/5", "usage:account:read:group/3/account/5", "usage:account:read:account/6", "account:read:account/5"}
	scopeRows(t, db, keys...)
	_, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "viewer", Name: lt("v", "v"), PermissionKeys: keys})
	require.NoError(t, err)
	holder := addUser(t, s, "h@x.com", "viewer")
	require.NoError(t, s.db.Tx(ctx, func(tx *gorm.DB) error {
		_, err := s.ClearScopeGrants(ctx, tx, root, core.ScopeRefAccount, []int64{5})
		return err
	}))
	s.Committed(ctx, bump(t, s))
	set, err := s.PermissionSet(ctx, holder)
	require.NoError(t, err)
	for _, k := range keys {
		require.Equal(t, k == "usage:account:read:account/6", set.Has(k), k)
	}
}
