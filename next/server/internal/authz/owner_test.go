package authz

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func ptr(v int64) *int64 { return &v }

// ownerOrNone is *p, 0 for nil.
func ownerOrNone(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// moveOwner changes the owner of user as iam's update does: Bump naming
// it, CheckOwner, the write of what it returns.
func moveOwner(s *Service, actor, user int64, owner *int64) (int64, error) {
	ctx := context.Background()
	var got, v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var err error
		if v, err = s.Bump(ctx, tx, user); err != nil {
			return err
		}
		if got, err = s.CheckOwner(ctx, tx, actor, user, false, owner); err != nil {
			return err
		}
		return tx.Exec(`UPDATE users SET owner_id = NULLIF(?::bigint, 0) WHERE id = ?`, got, user).Error
	})
	if err == nil {
		s.Committed(ctx, v)
	}
	return got, err
}

// createOwned creates a user as iam's create does: its id drawn,
// CheckOwner deciding its owner (nil: the default), inserted with it, its
// roles set.
func createOwned(s *Service, actor int64, email string, owner *int64, roles ...string) (int64, error) {
	ctx := context.Background()
	var id, v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Raw(`SELECT nextval(pg_get_serial_sequence('users', 'id'))`).Scan(&id).Error; err != nil {
			return err
		}
		got, err := s.CheckOwner(ctx, tx, actor, id, true, owner)
		if err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO users (id, email, password_hash, owner_id) VALUES (?, ?, 'x', NULLIF(?::bigint, 0))`, id, email, got).Error; err != nil {
			return err
		}
		v, err = s.SetUserRoles(ctx, tx, actor, id, roles)
		return err
	})
	if err == nil {
		s.Committed(ctx, v)
	}
	return id, err
}

// ownerIn is the owner of user as stored, 0 for none.
func ownerIn(t *testing.T, s *Service, user int64) int64 {
	t.Helper()
	var owner *int64
	require.NoError(t, s.db.Gorm.Raw(`SELECT owner_id FROM users WHERE id = ?`, user).Scan(&owner).Error)
	return ownerOrNone(owner)
}

func ownerAudits(t *testing.T, s *Service, user int64) []string {
	t.Helper()
	var out []string
	require.NoError(t, s.db.Gorm.Raw(`SELECT detail::text FROM audit_logs WHERE action = 'user.owner' AND target_id = ? ORDER BY id`, itoa(user)).Scan(&out).Error)
	return out
}

// The owner of a user (CONTRACTS §102.3, plan 5.3, 5.4): any live user an
// unscoped holder names, in a chain of core.MaxTeamDepth levels at most,
// never the user itself or one below it, and one that manages the user by
// its roles, also after later role changes; a team holder names itself
// alone. Every change is audited.
func TestCheckOwner(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	operator := addUser(t, s, "op@x.com", RoleOperator)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	plain := addUser(t, s, "plain@x.com", RoleUser)
	staff := addUser(t, s, "staff@x.com", RoleUser, RoleOperator)

	got, err := moveOwner(s, admin, plain, ptr(agent))
	require.NoError(t, err)
	require.Equal(t, agent, got)
	got, err = moveOwner(s, admin, plain, ptr(agent))
	require.NoError(t, err)
	require.Equal(t, agent, got)
	got, err = moveOwner(s, admin, plain, nil)
	require.NoError(t, err)
	require.Equal(t, agent, got)
	require.Equal(t, []string{`{"to": ` + itoa(agent) + `, "from": null}`}, ownerAudits(t, s, plain), "one change, one row")

	deleted := addUser(t, s, "gone@x.com", RoleUser)
	_, err = db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, deleted)
	require.NoError(t, err)
	for _, tc := range []struct {
		user, owner int64
		code        string
	}{
		{plain, plain, "self"},
		{plain, 1 << 40, "not_found"},
		{plain, deleted, "not_found"},
		{agent, plain, "cycle"},
		{staff, agent, "owner_cannot_manage"},
	} {
		_, err := moveOwner(s, admin, tc.user, ptr(tc.owner))
		require.Equal(t, tc.code, fieldCode(err, "owner_id"), "%d under %d: %v", tc.user, tc.owner, err)
	}

	// Roles the owner does not manage are refused while it owns the user
	// (and for a new user, whose roles come before CheckOwner).
	err = setRoles(s, admin, plain, RoleUser, RoleOperator)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "owner_id"), "%v", err)
	_, err = createOwned(s, admin, "new-op@x.com", ptr(agent), RoleUser, RoleOperator)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "owner_id"), "%v", err)
	require.NoError(t, setRoles(s, admin, plain, RoleUser, RoleSupplier))

	// D6: the operator acts on neither an agent nor its users.
	_, err = moveOwner(s, operator, addUser(t, s, "p2@x.com", RoleUser), ptr(agent))
	require.True(t, isCode(err, "permission_denied"), "%v", err)

	// The system names any owner, the rules on the owner aside.
	_, err = moveOwner(s, 0, plain, ptr(plain))
	require.Equal(t, "self", fieldCode(err, "owner_id"))
	got, err = moveOwner(s, 0, plain, ptr(0))
	require.NoError(t, err)
	require.Zero(t, got)
	_, err = moveOwner(s, admin, plain, ptr(agent))
	require.NoError(t, err)
}

// A team holder (no user:create or user:update for every user) makes
// itself the owner and nobody else, not even a user of its team or none;
// it defaults to itself on a create, the unscoped holder to none.
func TestCheckOwnerTeamHolder(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	sub := chain(t, s, agent, 1, RoleSupplierAgent)[0]
	leaf := chain(t, s, sub, 1, RoleUser)[0]
	peer := chain(t, s, agent, 1, RoleUser)[0]

	for _, owner := range []int64{peer, 0, admin} {
		_, err := moveOwner(s, agent, leaf, ptr(owner))
		require.True(t, isCode(err, "permission_denied"), "%d: %v", owner, err)
	}
	got, err := moveOwner(s, agent, leaf, ptr(agent))
	require.NoError(t, err)
	require.Equal(t, agent, got)
	require.Equal(t, []string{`{"to": ` + itoa(agent) + `, "from": ` + itoa(sub) + `}`}, ownerAudits(t, s, leaf))
	// Back under sub: the agent may not name it.
	_, err = moveOwner(s, agent, leaf, ptr(sub))
	require.True(t, isCode(err, "permission_denied"), "%v", err)

	created, err := createOwned(s, agent, "made@x.com", ptr(agent), RoleUser, RoleSupplier)
	require.NoError(t, err)
	require.Equal(t, []string{`{"to": ` + itoa(agent) + `, "from": null}`}, ownerAudits(t, s, created))
	_, err = createOwned(s, agent, "made2@x.com", ptr(sub), RoleUser)
	require.True(t, isCode(err, "permission_denied"), "%v", err)
	// Whether it may name the owner comes first: a user outside its team,
	// or none at all, is refused alike.
	for _, owner := range []int64{admin, 1 << 40} {
		_, err = createOwned(s, agent, "made3@x.com", ptr(owner), RoleUser)
		require.True(t, isCode(err, "permission_denied"), "%d: %v", owner, err)
		_, err = moveOwner(s, agent, leaf, ptr(owner))
		require.True(t, isCode(err, "permission_denied"), "%d: %v", owner, err)
	}

	// nil on a create: the team holder itself, none for the unscoped one.
	for actor, want := range map[int64]int64{agent: agent, admin: 0} {
		id, err := createOwned(s, actor, "nil"+itoa(actor)+"@x.com", nil, RoleUser)
		require.NoError(t, err)
		require.Equal(t, want, ownerIn(t, s, id))
	}
}

// A chain is at most core.MaxTeamDepth levels below its top: no owner
// that would put the user, or a user below it, deeper (plan 5.3).
func TestCheckOwnerDepth(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	c := chain(t, s, 0, core.MaxTeamDepth+1, RoleUser) // c[i] is i levels below c[0]
	last := c[core.MaxTeamDepth]

	_, err := createOwned(s, admin, "deep@x.com", ptr(last), RoleUser)
	require.Equal(t, "too_deep", fieldCode(err, "owner_id"), "%v", err)
	_, err = createOwned(s, admin, "ok@x.com", ptr(c[core.MaxTeamDepth-1]), RoleUser)
	require.NoError(t, err)

	pair := chain(t, s, 0, 2, RoleUser) // a user with one below it
	_, err = moveOwner(s, admin, pair[0], ptr(c[core.MaxTeamDepth-1]))
	require.Equal(t, "too_deep", fieldCode(err, "owner_id"), "%v", err)
	_, err = moveOwner(s, admin, pair[0], ptr(c[core.MaxTeamDepth-2]))
	require.NoError(t, err)
}

// Two moves that would make a cycle together are serialized by the authz
// lock: one of them sees the other and is refused (plan 5.3).
func TestCheckOwnerConcurrentCycle(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	a := addUser(t, s, "a@x.com", RoleUser)
	b := addUser(t, s, "b@x.com", RoleUser)
	errs := make(chan error, 2)
	go func() { _, err := moveOwner(s, admin, a, ptr(b)); errs <- err }()
	go func() { _, err := moveOwner(s, admin, b, ptr(a)); errs <- err }()
	var codes []string
	for range 2 {
		codes = append(codes, fieldCode(<-errs, "owner_id"))
	}
	require.ElementsMatch(t, []string{"", "cycle"}, codes)
}

// A move changes the teams of every user above the old and the new owner:
// the actor must manage each of them, but for itself and those above it
// (CONTRACTS §102.3). An operator may move a plain user into or out of an
// agent's team neither directly nor through a supplier of that team (D6);
// a sub-agent still owns the users it creates though its own owner is
// above it.
func TestCheckOwnerChains(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	operator := addUser(t, s, "op@x.com", RoleOperator)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	supplier := chain(t, s, agent, 1, RoleUser, RoleSupplier)[0]
	plain := addUser(t, s, "plain@x.com", RoleUser)
	loose := addUser(t, s, "loose@x.com", RoleUser, RoleSupplier)

	// The operator manages the supplier and the user, not the agent above.
	require.NoError(t, s.CanActOnUsers(context.Background(), operator, core.ActOnIdentity, supplier, plain))
	_, err := moveOwner(s, operator, plain, ptr(supplier))
	require.True(t, isCode(err, "permission_denied"), "into the agent's team: %v", err)
	_, err = createOwned(s, operator, "new@x.com", ptr(supplier), RoleUser)
	require.True(t, isCode(err, "permission_denied"), "created into the agent's team: %v", err)
	_, err = moveOwner(s, admin, plain, ptr(supplier))
	require.NoError(t, err)
	_, err = moveOwner(s, operator, plain, ptr(0))
	require.True(t, isCode(err, "permission_denied"), "out of the agent's team: %v", err)
	_, err = moveOwner(s, operator, plain, ptr(loose))
	require.True(t, isCode(err, "permission_denied"), "out of the agent's team: %v", err)
	require.Equal(t, supplier, ownerIn(t, s, plain))
	// Outside any agent's team the operator still moves users.
	_, err = moveOwner(s, operator, addUser(t, s, "p2@x.com", RoleUser), ptr(loose))
	require.NoError(t, err)

	// A sub-agent below the agent: what it creates and moves stays in the
	// agent's team, which the sub-agent does not manage.
	sub := chain(t, s, agent, 1, RoleSupplierAgent)[0]
	made, err := createOwned(s, sub, "made@x.com", nil, RoleUser, RoleSupplier)
	require.NoError(t, err)
	leaf := chain(t, s, made, 1, RoleUser)[0]
	_, err = moveOwner(s, sub, leaf, ptr(sub))
	require.NoError(t, err)
	require.Equal(t, sub, ownerIn(t, s, leaf))
}

// A role write that leaves an owner unable to manage one of its live users
// by their roles is refused, on the write's field (CONTRACTS §102.3):
// more for the users' role, less for the owner's, a reset of either. Users
// without an owner, and deleted ones, do not count.
func TestRoleWritesKeepOwners(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	agent := addUser(t, s, "agent@x.com", RoleSupplierAgent)
	addUser(t, s, "loose@x.com", RoleUser, RoleSupplier)
	supplierRole, agentRole := roleID(t, s, RoleSupplier), roleID(t, s, RoleSupplierAgent)
	keysOf := func(id int64) []string {
		t.Helper()
		r, err := s.GetRole(ctx, id)
		require.NoError(t, err)
		return r.PermissionKeys
	}
	supplierKeys, agentKeys := keysOf(supplierRole), keysOf(agentRole)

	// No owned supplier yet: the supplier role may grow beyond the agent.
	_, err := s.SetRolePermissions(ctx, admin, supplierRole, append(slices.Clone(supplierKeys), "user:delete"), nil)
	require.NoError(t, err)
	_, err = s.SetRolePermissions(ctx, admin, supplierRole, supplierKeys, nil)
	require.NoError(t, err)

	sub := chain(t, s, agent, 1, RoleUser, RoleSupplier)[0]
	_, err = s.SetRolePermissions(ctx, admin, supplierRole, append(slices.Clone(supplierKeys), "user:delete"), nil)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "permission_keys"), "%v", err)
	require.Equal(t, sub, err.(*core.Error).Details["user_id"])
	_, err = s.SetRoleAssignable(ctx, admin, agentRole, AssignableInput{RoleKeys: []string{RoleSupplierAgent}})
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "role_keys"), "%v", err)
	_, err = s.SetRolePermissions(ctx, admin, agentRole, slices.DeleteFunc(slices.Clone(agentKeys), func(k string) bool { return k == "proxy:manage:own" }), nil)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "permission_keys"), "%v", err)

	// The plugin matrix judges its roles once all are written: the
	// supplier's grant alone is refused, with the agent's too it passes.
	require.NoError(t, syncer(t, s, "m")(core.PermissionDef{Key: "flow:read", GrantRoles: unscoped(RoleAdmin)}))
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleSupplier: {"plugin.m:flow:read"}}, nil)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "roles."+RoleSupplier), "%v", err)
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleSupplier: {"plugin.m:flow:read"}, RoleSupplierAgent: {"plugin.m:flow:read"}}, nil)
	require.NoError(t, err)
	agentKeys = keysOf(agentRole)

	// Both roles gain user:delete, then a reset takes it from the agent's.
	_, err = s.SetRolePermissions(ctx, admin, agentRole, append(slices.Clone(agentKeys), "user:delete"), nil)
	require.NoError(t, err)
	_, err = s.SetRolePermissions(ctx, admin, supplierRole, append(slices.Clone(supplierKeys), "user:delete"), nil)
	require.NoError(t, err)
	_, err = s.ResetRole(ctx, admin, agentRole, nil)
	require.Equal(t, "owner_cannot_manage", fieldCode(err, "permission_keys"), "%v", err)
	require.Contains(t, keysOf(agentRole), "user:delete", "the refused reset changed nothing")

	// A deleted user is no one's to manage.
	_, err = db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, sub)
	require.NoError(t, err)
	_, err = s.ResetRole(ctx, admin, agentRole, nil)
	require.NoError(t, err)
}
