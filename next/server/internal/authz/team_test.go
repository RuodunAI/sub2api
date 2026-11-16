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

// setOwner makes owner (0: none) the owner of user, as a raw write the
// version records and this node takes in.
func setOwner(t *testing.T, s *Service, user, owner int64) {
	t.Helper()
	s.Committed(context.Background(), bumpWith(t, s, `UPDATE users SET owner_id = NULLIF(?::bigint, 0) WHERE id = ?`, owner, user))
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

// A team is every user below its holder, core.MaxTeamDepth levels at most,
// soft-deleted ones included (D7), sorted and without the holder; it is
// cached for the authz version it was read at and read again for a newer
// one; one that cannot be read at the version asked for is an error,
// never empty.
func TestTeam(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	top := addUser(t, s, "top@x.com", RoleSupplierAgent)
	c := chain(t, s, top, core.MaxTeamDepth+2)
	side := addUser(t, s, "side@x.com", RoleSupplier)
	setOwner(t, s, side, c[0])
	_, err := db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, side)
	require.NoError(t, err)
	v := s.Version()

	team, err := s.Team(ctx, top, v)
	require.NoError(t, err)
	want := append([]int64{side}, c[:core.MaxTeamDepth]...)
	require.ElementsMatch(t, want, team, "eight levels, the deleted user included")
	require.IsIncreasing(t, team)
	team, err = s.Team(ctx, c[core.MaxTeamDepth], v)
	require.NoError(t, err)
	require.Equal(t, []int64{c[core.MaxTeamDepth+1]}, team)

	// A write the version does not record is not seen at that version (the
	// team is cached), and is once the version moves on.
	loner := addUser(t, s, "loner@x.com", RoleSupplier)
	v = s.Version()
	team, err = s.Team(ctx, c[core.MaxTeamDepth+1], v)
	require.NoError(t, err)
	require.Empty(t, team)
	_, err = db.SQL.ExecContext(ctx, `UPDATE users SET owner_id = $1 WHERE id = $2`, c[core.MaxTeamDepth+1], loner)
	require.NoError(t, err)
	team, err = s.Team(ctx, c[core.MaxTeamDepth+1], v)
	require.NoError(t, err)
	require.Empty(t, team, "cached at the version")
	s.Committed(ctx, bump(t, s))
	team, err = s.Team(ctx, c[core.MaxTeamDepth+1], s.Version())
	require.NoError(t, err)
	require.Equal(t, []int64{loner}, team)

	// A version the database has not reached: unavailable, not empty.
	_, err = s.Team(ctx, top, s.Version()+5)
	require.True(t, isCode(err, core.ErrUnavailable.Code), "%v", err)
	_, err = New(Deps{}).Team(ctx, top, 1)
	require.Error(t, err)
}

// core.Scope (and Router.PermScoped, through the same ResolveScope) reach
// the rows of the holder's team with a team scope: accounts owned by a user
// below it, users below it (not itself), never anyone else's; an access
// key narrows team to own before it is resolved (CONTRACTS §103.2).
func TestTeamScope(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	sub := chain(t, s, agent, 2, RoleSupplier)
	other := addUser(t, s, "other@x.com", RoleSupplierAgent)
	stranger := chain(t, s, other, 1, RoleSupplier)[0]
	ctx := core.WithUserID(context.Background(), agent)

	accounts, err := core.Scope(ctx, s, perm.AccountRead)
	require.NoError(t, err)
	require.Equal(t, append([]int64{agent}, sub...), accounts.Owners)
	require.False(t, accounts.All)
	usage, err := core.Scope(ctx, s, perm.UsageAccountRead)
	require.NoError(t, err)
	require.Equal(t, accounts.Owners, usage.Owners)
	update, err := core.Scope(ctx, s, perm.AccountUpdate)
	require.NoError(t, err)
	require.Equal(t, []int64{agent}, update.Owners, "own stays own")

	users, err := core.Scope(ctx, s, perm.UserRead)
	require.NoError(t, err)
	var seen []int64
	require.NoError(t, db.Gorm.Table("users u").Scopes(core.UserRows().Scope(users)).Order("u.id").Pluck("u.id", &seen).Error)
	require.Equal(t, sub, seen, "the users below, not the agent, not another agent's")
	require.NotContains(t, seen, stranger)

	keyed := core.WithAccessKey(ctx, core.AccessKeyIdentity{UserID: agent, Permissions: []string{"account:read:own", "user:read"}})
	accounts, err = core.Scope(keyed, s, perm.AccountRead)
	require.NoError(t, err)
	require.Equal(t, []int64{agent}, accounts.Owners)
	users, err = core.Scope(keyed, s, perm.UserRead)
	require.NoError(t, err)
	require.Equal(t, append([]int64{agent}, sub...), users.Owners)
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
