package authz

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// countingLoader counts the reads of the policy and of each user's roles.
type countingLoader struct {
	casbinx.Loader
	policy atomic.Int64
	users  map[int64]*atomic.Int64
}

func newCountingLoader(db *store.DB, uids ...int64) *countingLoader {
	l := &countingLoader{Loader: casbinx.NewLoader(db), users: map[int64]*atomic.Int64{}}
	for _, id := range uids {
		l.users[id] = &atomic.Int64{}
	}
	return l
}

func (l *countingLoader) Policy(ctx context.Context) (casbinx.Policy, error) {
	l.policy.Add(1)
	return l.Loader.Policy(ctx)
}

func (l *countingLoader) UserRoles(ctx context.Context, uid int64) (casbinx.UserRoles, error) {
	if n := l.users[uid]; n != nil {
		n.Add(1)
	}
	return l.Loader.UserRoles(ctx, uid)
}

// counted makes s resolve through a countingLoader of uids.
func counted(s *Service, uids ...int64) *countingLoader {
	l := newCountingLoader(s.db, uids...)
	s.casbin = casbinx.NewResolver(l, casbinx.NewRoleCache(l, nil), SelfPermissionKeys())
	return l
}

// D1: assigning a role (or changing a user's status) moves the authz
// version only. The policy snapshot is not rebuilt, and on this node and on
// every other one only that user's set is read again; a policy write (a
// role's grants) rebuilds the snapshot and drops every set.
func TestUserWriteKeepsPolicySnapshot(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	bus := newMemBus()
	s := newService(t, db, bus, nil)
	other := newService(t, db, bus, nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	a := addUser(t, s, "a@x.com", RoleOperator)
	b := addUser(t, s, "b@x.com", RoleUser)
	nodes := map[string]*Service{"writer": s, "other": other}
	loaders := map[string]*countingLoader{}
	for name, n := range nodes {
		loaders[name] = counted(n, a, b)
		for _, uid := range []int64{a, b} {
			_, err := n.PermissionSet(ctx, uid)
			require.NoError(t, err)
		}
	}
	reads := func(name string) (policy, ua, ub int64) {
		l := loaders[name]
		return l.policy.Load(), l.users[a].Load(), l.users[b].Load()
	}
	for name := range nodes {
		p, ua, ub := reads(name)
		require.Equal(t, [3]int64{1, 1, 1}, [3]int64{p, ua, ub}, name)
	}
	policyBefore := policyVersion(t, s)

	// b becomes an operator: a user write.
	require.NoError(t, setRoles(s, root, b, RoleOperator))
	require.Equal(t, policyBefore, policyVersion(t, s), "a role assignment leaves the policy version")
	for name, n := range nodes {
		setA, err := n.PermissionSet(ctx, a)
		require.NoError(t, err)
		setB, err := n.PermissionSet(ctx, b)
		require.NoError(t, err)
		require.Equal(t, setA.KeyList(), setB.KeyList(), "%s: b holds what an operator holds", name)
		require.Equal(t, n.Version(), setA.Version, "%s: a's kept set is labelled with the node's version", name)
		p, ua, ub := reads(name)
		require.Equal(t, [3]int64{1, 1, 2}, [3]int64{p, ua, ub}, "%s: b read again, a kept, no snapshot rebuilt", name)
	}

	// A status change: the same.
	require.NoError(t, commitBump(ctx, s, a))
	for name, n := range nodes {
		_, err := n.PermissionSet(ctx, a)
		require.NoError(t, err)
		_, err = n.PermissionSet(ctx, b)
		require.NoError(t, err)
		p, ua, ub := reads(name)
		require.Equal(t, [3]int64{1, 2, 2}, [3]int64{p, ua, ub}, "%s: a read again only", name)
	}

	// A role's grants: the snapshot is rebuilt and every set read again.
	op := roleID(t, s, RoleOperator)
	_, err := s.SetRolePermissions(ctx, root, op, rolePermissionKeys(t, s, op)[1:], nil)
	require.NoError(t, err)
	require.Greater(t, policyVersion(t, s), policyBefore)
	for name, n := range nodes {
		for _, uid := range []int64{a, b} {
			_, err := n.PermissionSet(ctx, uid)
			require.NoError(t, err)
		}
		p, ua, ub := reads(name)
		require.Equal(t, [3]int64{2, 3, 3}, [3]int64{p, ua, ub}, name)
	}
}

// A broadcast a node misses (a gap in the versions it heard of) or one
// that does not say what changed drops every set: only a version next to
// the newest known one drops just its users.
func TestMissedBroadcastDropsEverySet(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, nil, nil)
	ctx := context.Background()
	a := addUser(t, s, "a@x.com", RoleOperator)
	b := addUser(t, s, "b@x.com", RoleUser)
	l := counted(s, a, b)
	warm := func() {
		for _, uid := range []int64{a, b} {
			_, err := s.PermissionSet(ctx, uid)
			require.NoError(t, err)
		}
	}
	warm()
	v := s.Version()

	s.invalidate(authzChange{Version: v + 1, Users: []int64{b}})
	warm()
	require.Equal(t, [2]int64{1, 2}, [2]int64{l.users[a].Load(), l.users[b].Load()}, "next version: b only")

	s.invalidate(authzChange{Version: v + 3, Users: []int64{b}})
	warm()
	require.Equal(t, [2]int64{2, 3}, [2]int64{l.users[a].Load(), l.users[b].Load()}, "a version skipped: every set")

	s.invalidate(authzChange{Version: v + 4})
	warm()
	require.Equal(t, [2]int64{3, 4}, [2]int64{l.users[a].Load(), l.users[b].Load()}, "a change naming nobody: every set")

	c, ok := parseChange([]byte("12"))
	require.True(t, ok)
	require.Equal(t, authzChange{Version: 12}, c, "a bare version reads as a change of everything")
	c, ok = parseChange([]byte(`{"v":13,"u":[4]}`))
	require.True(t, ok)
	require.True(t, c.only())
	_, ok = parseChange([]byte("nope"))
	require.False(t, ok)
}

// commitBump runs a user write of uid (a status change) and commits it.
func commitBump(ctx context.Context, s *Service, uid int64) error {
	var v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var err error
		v, err = s.Bump(ctx, tx, uid)
		return err
	})
	if err == nil {
		s.Committed(ctx, v)
	}
	return err
}

func policyVersion(t *testing.T, s *Service) int64 {
	t.Helper()
	_, p, err := s.currentVersions(context.Background())
	require.NoError(t, err)
	return p
}
