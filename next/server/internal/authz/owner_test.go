package authz

import (
	"context"
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

// createOwned creates a user as iam's create does: inserted with owner
// (nil: none), its roles set, then CheckOwner.
func createOwned(s *Service, actor int64, email string, owner *int64, roles ...string) (int64, error) {
	ctx := context.Background()
	var id, v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Raw(`INSERT INTO users (email, password_hash, owner_id) VALUES (?, 'x', NULLIF(?::bigint, 0)) RETURNING id`, email, ownerOrNone(owner)).Scan(&id).Error; err != nil {
			return err
		}
		var err error
		if v, err = s.SetUserRoles(ctx, tx, actor, id, roles); err != nil {
			return err
		}
		got, err := s.CheckOwner(ctx, tx, actor, id, true, owner)
		if err == nil && got != ownerOrNone(owner) {
			err = tx.Exec(`UPDATE users SET owner_id = ? WHERE id = ?`, got, id).Error
		}
		return err
	})
	if err == nil {
		s.Committed(ctx, v)
	}
	return id, err
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
	agent := addUser(t, s, "agent@x.com", RoleUser, RoleSupplierAgent)
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
	ctx := context.Background()
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	agent := addUser(t, s, "agent@x.com", RoleUser, RoleSupplierAgent)
	sub := chain(t, s, agent, 1, RoleUser, RoleSupplierAgent)[0]
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
	_, err = createOwned(s, agent, "made3@x.com", nil, RoleUser)
	require.True(t, isCode(err, "permission_denied"), "outside its team before the owner: %v", err)

	// nil on a create: the team holder itself, none for the unscoped one.
	for actor, want := range map[int64]int64{agent: agent, admin: 0} {
		require.NoError(t, s.db.Tx(ctx, func(tx *gorm.DB) error {
			var id int64
			if err := tx.Raw(`INSERT INTO users (email, password_hash) VALUES (?, 'x') RETURNING id`, "nil"+itoa(actor)+"@x.com").Scan(&id).Error; err != nil {
				return err
			}
			if _, err := s.Bump(ctx, tx, id); err != nil {
				return err
			}
			got, err := s.CheckOwner(ctx, tx, actor, id, true, nil)
			require.Equal(t, want, got)
			return err
		}))
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
