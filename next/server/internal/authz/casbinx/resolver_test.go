package casbinx

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRoleCache is a RoleCache over a Loader without L2 (RoleCache proper is
// rolecache.go's): Warm reads the loader each time, active users getting
// SelfRole. evict drops the entry right after that many Warms, to stand for
// an eviction between Warm and the resolution.
type fakeRoleCache struct {
	*mapRoles
	l     Loader
	warms atomic.Int64
	evict int64
}

func newFakeRoleCache(l Loader) *fakeRoleCache { return &fakeRoleCache{mapRoles: newMapRoles(), l: l} }

func (c *fakeRoleCache) Warm(ctx context.Context, uid, _ int64) (UserRoles, error) {
	ur, err := c.l.UserRoles(ctx, uid)
	if err != nil {
		return UserRoles{}, err
	}
	roles := slices.Clone(ur.Roles)
	if ur.Active {
		roles = append(roles, SelfRole)
	} else {
		roles = nil
	}
	c.set(UserSub(uid), roles...)
	if c.warms.Add(1) <= c.evict {
		c.drop(UserSub(uid))
	}
	return ur, nil
}

func TestResolverGrants(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: fixed(4), users: map[int64]UserRoles{
		1: {Version: 6, Active: true, Roles: []string{"r:t"}},
		2: {Version: 6},
		3: {Version: 3, Active: true, Roles: []string{"r:t"}},
	}}
	r := NewResolver(l, newFakeRoleCache(l), []string{"self:profile"}, WithWait(time.Second), WithDebounce(time.Millisecond))
	ctx := context.Background()
	set, err := r.Grants(ctx, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.KeyList(); !slices.Equal(got, []string{"account:read", "self:profile"}) || set.Version != 4 {
		t.Fatalf("Grants = %v at %d", got, set.Version)
	}
	if set, err := r.Grants(ctx, 2, 3); err != nil || len(set.KeyList()) != 0 || set.Version != 4 {
		t.Fatalf("inactive user: %+v, %v", set, err)
	}
	if ok, err := r.Enforce(ctx, 1, "account:read:own", 3); !ok || err != nil {
		t.Fatalf("Enforce = %v, %v", ok, err)
	}
	if ok, err := r.Enforce(ctx, 2, "self:profile", 3); ok || err != nil {
		t.Fatalf("Enforce of an inactive user = %v, %v", ok, err)
	}
	// Roles read below min: fail closed.
	if _, err := r.Grants(ctx, 3, 4); !errors.Is(err, ErrStale) {
		t.Fatalf("roles below min: %v", err)
	}
	// Snapshot below min, never reaching it: fail closed.
	if _, err := r.Grants(ctx, 1, 5); !errors.Is(err, ErrStale) {
		t.Fatalf("snapshot below min: %v", err)
	}
}

// An entry evicted between Warm and the resolution is warmed once more;
// evicted again, the resolution fails with ErrNotWarm rather than answer an
// empty set.
func TestResolverRewarmsOnce(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: fixed(1), users: map[int64]UserRoles{1: {Version: 1, Active: true, Roles: []string{"r:t"}}}}
	rc := newFakeRoleCache(l)
	rc.evict = 1
	r := NewResolver(l, rc, nil)
	if set, err := r.Grants(context.Background(), 1, 1); err != nil || !set.Has("account:read") {
		t.Fatalf("Grants = %+v, %v", set, err)
	}
	if n := rc.warms.Load(); n != 2 {
		t.Fatalf("%d warms, want 2", n)
	}
	rc.evict = 100
	if _, err := r.Grants(context.Background(), 1, 1); !errors.Is(err, ErrNotWarm) {
		t.Fatalf("Grants = %v, want ErrNotWarm", err)
	}
	if _, err := r.Enforce(context.Background(), 1, "account:read", 1); !errors.Is(err, ErrNotWarm) {
		t.Fatalf("Enforce = %v, want ErrNotWarm", err)
	}
}
