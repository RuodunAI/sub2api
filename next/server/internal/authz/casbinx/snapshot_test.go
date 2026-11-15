package casbinx

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// core TestPermissionSetScoped through Casbin: Enforce and the set Grants
// returns answer each key as core's set does.
func TestPermissionSetScopedViaCasbin(t *testing.T) {
	t.Parallel()
	keys := []string{"account:read:own", "account:update:group/12", "proxy:read"}
	s := single(t, keys...)
	set := grants(t, s, 1)
	for k, want := range map[string]bool{
		"account:read":                      false,
		"account:read:own":                  true,
		"account:own:read":                  false,
		"account:read:*":                    false,
		"account:update:group/12":           true,
		"account:update:group/12/account/5": true,
		"account:update:account/5":          false,
		"account:update:group/13":           false,
		"proxy:read:proxy/3":                true,
		"proxy:read":                        true,
		"account:update:group/x":            false,
		"account:delete:own":                false,
	} {
		if got := enforce(t, s, 1, k); got != want {
			t.Errorf("Enforce(%s) = %v, want %v", k, got, want)
		}
		if got := set.Has(k); got != want {
			t.Errorf("Grants.Has(%s) = %v, want %v", k, got, want)
		}
	}
	if !slices.Equal(set.KeyList(), core.PermissionSetOf(keys...).KeyList()) {
		t.Fatalf("KeyList = %v", set.KeyList())
	}
}

// A superuser row holds every key, those added after the snapshot was built
// included (a plugin installed later), as core's Superuser flag does; it
// loads whatever the catalog says.
func TestSuperuserFutureKeys(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:super_admin")
	roles.set(UserSub(2))
	s, err := Build(Policy{Version: 3, Rules: []Rule{superRow("r:super_admin")}}, Options{Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"settings:manage", "plugin.newly_installed:x:manage:flow/3", "account:read:group/*"} {
		if !enforce(t, s, 1, k) {
			t.Errorf("superuser lacks %s", k)
		}
	}
	if set := grants(t, s, 1); !set.Superuser || set.Version != 3 || set.Keys == nil {
		t.Fatalf("Grants = %+v", set)
	}
	if enforce(t, s, 2, "settings:manage") {
		t.Fatal("a user without roles holds something")
	}
}

// Grants of actions that are not active (plugin disabled, permission
// removed) stay stored but are not loaded; rows of no catalog action, rows
// with an empty column and g rows are not loaded either.
func TestInactivePermissionsNotLoaded(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:t")
	rules := []Rule{
		grant("r:t", "plugin.m:f:m:flow/2"), grant("r:t", "account:read"), grant("r:t", "proxy:read"),
		{"p", "r:t", "account:update", ""}, {"g", "u:1", "r:other", ""}, grant("r:other", "settings:manage"),
	}
	active := map[string]bool{"account:read": true, "account:update": true, "settings:manage": true}
	s, err := Build(Policy{Version: 1, Active: active, Rules: rules}, Options{Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	set := grants(t, s, 1)
	if got := set.KeyList(); !slices.Equal(got, []string{"account:read"}) {
		t.Fatalf("Grants = %v", got)
	}
	for k, want := range map[string]bool{"plugin.m:f:m:flow/2": false, "account:read:own": true, "proxy:read": false,
		"account:update:own": false, "settings:manage": false} {
		if got := enforce(t, s, 1, k); got != want {
			t.Errorf("Enforce(%s) = %v, want %v", k, got, want)
		}
	}
}

// The self permissions are SelfRole's: held by a user whose source entry
// names it (an active one), by no other; no entry at all is ErrNotWarm,
// not an empty set.
func TestSelfPermissions(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:t", SelfRole)
	roles.set(UserSub(2)) // inactive
	self := []string{"self:profile", "self:security"}
	s, err := Build(Policy{Version: 1, Rules: []Rule{grant("r:t", "proxy:read")}, Active: map[string]bool{"proxy:read": true}},
		Options{Roles: roles, Self: self})
	if err != nil {
		t.Fatal(err)
	}
	if got := grants(t, s, 1).KeyList(); !slices.Equal(got, []string{"proxy:read", "self:profile", "self:security"}) {
		t.Fatalf("active user: %v", got)
	}
	if set := grants(t, s, 2); len(set.KeyList()) != 0 || set.Superuser || set.Keys == nil {
		t.Fatalf("inactive user: %+v", set)
	}
	if enforce(t, s, 2, "self:profile") || !enforce(t, s, 1, "self:profile") {
		t.Fatal("self permission held by the wrong user")
	}
	if _, err := s.Grants(3); !errors.Is(err, ErrNotWarm) {
		t.Fatalf("Grants of a cold user: %v", err)
	}
	if _, err := s.Enforce(3, "self:profile"); !errors.Is(err, ErrNotWarm) {
		t.Fatalf("Enforce of a cold user: %v", err)
	}
	if _, err := Build(Policy{}, Options{Roles: roles, Self: []string{"self:x:own"}}); err == nil {
		t.Fatal("an invalid self key was taken")
	}
	if _, err := Build(Policy{}, Options{}); err == nil {
		t.Fatal("Build without a RoleSource")
	}
}

// authz TestCanGrant / core Missing on Casbin-built sets: the
// anti-escalation comparison stays ours, fed by Grants.
func TestCanGrantOnCasbinGrants(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:a")
	roles.set(UserSub(2), "r:super_admin")
	roles.set(UserSub(3), "r:b")
	s := build(t, roles, grant("r:a", "user:read"), grant("r:a", "user:update"), superRow("r:super_admin"), grant("r:b", "user:read"))
	for _, tc := range []struct {
		actor int64
		perms []string
		deny  bool
	}{
		{2, []string{"user:read", "user:update", "settings:manage"}, false},
		{1, []string{"user:read"}, false},
		{1, []string{"user:read", "user:update"}, false},
		{3, []string{"user:update"}, true},
		{3, []string{"user:read", "user:update"}, true},
		{1, nil, false},
	} {
		m := grants(t, s, tc.actor).Missing(core.PermissionSetOf(tc.perms...))
		if !m.Empty() != tc.deny {
			t.Errorf("actor %d grants %v: missing %+v", tc.actor, tc.perms, m)
		}
	}
}

// core TestRestrictScoped on a Casbin-built set: Restrict (access keys)
// stays ours and answers the same.
func TestRestrictScopedOnCasbinGrants(t *testing.T) {
	t.Parallel()
	user := grants(t, single(t, "account:read:own", "account:update:group/12", "account:delete", "proxy:read:own"), 1)
	for _, tc := range []struct{ key, want []string }{
		{[]string{"account:read"}, []string{"account:read:own"}},
		{[]string{"account:update"}, []string{"account:update:group/12"}},
		{[]string{"account:update:group/12/account/5"}, []string{"account:update:group/12/account/5"}},
		{[]string{"account:update:own"}, nil},
		{[]string{"account:update:group/*"}, []string{"account:update:group/12"}},
		{[]string{"account:delete:group/3", "proxy:read"}, []string{"account:delete:group/3", "proxy:read:own"}},
		{[]string{"account:test", "bogus:key:own"}, nil},
	} {
		got := user.Restrict(tc.key).KeyList()
		if len(got) == 0 {
			got = nil
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Restrict(%v) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// Why Restrict cannot become a matcher "user may AND key allows": the user
// holds account/5, the access key names group/12; both imply
// group/12/account/5, but Restrict drops scopes neither implies rather than
// intersecting them, so the key reaches nothing there.
func TestRestrictIsNotAMatcherIntersection(t *testing.T) {
	t.Parallel()
	s := single(t, "account:update:account/5")
	want := "account:update:group/12/account/5"
	userOK := enforce(t, s, 1, want)
	keyOK := PermMatch("account:update", "group/12", want)
	restricted := grants(t, s, 1).Restrict([]string{"account:update:group/12"}).Has(want)
	t.Logf("user=%v key=%v intersection=%v Restrict=%v", userOK, keyOK, userOK && keyOK, restricted)
	if !userOK || !keyOK || restricted {
		t.Fatal("the counterexample no longer holds")
	}
}

// core TestPermissionSetPluginScopes: plugin keys through Casbin.
func TestPluginScopesViaCasbin(t *testing.T) {
	t.Parallel()
	const a = "plugin.m:f:m"
	s := single(t, a+":flow/2")
	for k, want := range map[string]bool{a + ":flow/2": true, a + ":flow/3": false, a: false} {
		if got := enforce(t, s, 1, k); got != want {
			t.Errorf("Enforce(%s) = %v", k, got)
		}
	}
	if got := grants(t, s, 1).KeyList(); !slices.Equal(got, []string{a + ":flow/2"}) {
		t.Fatalf("Grants = %v", got)
	}
}

// The shadow comparison, offline: random roles over the vector keys and
// random users. For every key Enforce must answer as the set built
// directly from the user's roles (core's PermissionSet.Has), and Grants
// must hold the same keys and superuser flag.
func TestShadowEquivalenceRandom(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	var pool []string
	for _, c := range v.Implies {
		pool = append(pool, c.Have, c.Want)
	}
	pool = append(pool, "user:read", "settings:manage", "plugin.m:f:m:flow/2", "plugin.m:f:m")
	queries := append(slices.Clone(pool), "bogus", "account:read:group/x", "plugin.x:y:z:flow/1", "self:profile")
	self := []string{"self:profile"}
	r := rand.New(rand.NewPCG(1, 2))
	checks := 0
	for range 300 {
		roleKeys := map[string][]string{}
		var rules []Rule
		for ri := range 4 {
			role := RoleSub(string(rune('a' + ri)))
			for range r.IntN(6) {
				key := pool[r.IntN(len(pool))]
				roleKeys[role] = append(roleKeys[role], key)
				rules = append(rules, grant(role, key))
			}
		}
		superRole := r.IntN(10) == 0
		if superRole {
			rules = append(rules, superRow("r:su"))
		}
		roles := newMapRoles()
		want := map[int64]core.PermissionSet{}
		for uid := int64(1); uid <= 5; uid++ {
			set := core.PermissionSet{Keys: map[string]struct{}{}}
			active := r.IntN(8) != 0
			var held []string
			for _, role := range []string{"r:a", "r:b", "r:c", "r:d", "r:su"} {
				if !active || r.IntN(3) != 0 {
					continue
				}
				held = append(held, role)
				set.Superuser = set.Superuser || (role == "r:su" && superRole)
				for _, k := range roleKeys[role] {
					set.Add(k)
				}
			}
			if active {
				held = append(held, SelfRole)
				for _, k := range self {
					set.Add(k)
				}
			}
			roles.set(UserSub(uid), held...)
			want[uid] = set
		}
		s, err := Build(Policy{Version: 1, Active: activeOf(rules), Rules: rules}, Options{Roles: roles, Self: self})
		if err != nil {
			t.Fatal(err)
		}
		for uid, set := range want {
			for _, q := range queries {
				if got := enforce(t, s, uid, q); got != set.Has(q) {
					t.Fatalf("user %d %q: casbin %v, core %v", uid, q, got, set.Has(q))
				}
				checks++
			}
			g := grants(t, s, uid)
			if g.Superuser != set.Superuser || !slices.Equal(g.KeyList(), set.KeyList()) {
				t.Fatalf("user %d grants %v (super %v), core %v (super %v)", uid, g.KeyList(), g.Superuser, set.KeyList(), set.Superuser)
			}
		}
	}
	t.Logf("%d shadow comparisons, 0 mismatches", checks)
}

// Casbin's g memo lives in the compiled matcher, as long as the enforcer:
// with roles that change in the RoleSource it must be off, or a revoked
// role keeps working. newEnforcer turns it off; turned back on, the revoked
// role passes.
func TestGFunctionCacheMustBeOff(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:admin")
	s := build(t, roles, grant("r:admin", "settings:manage"))
	before := enforce(t, s, 1, "settings:manage")
	roles.set(UserSub(1)) // revoked in the cache
	after := enforce(t, s, 1, "settings:manage")
	if !before || after {
		t.Fatalf("memo off: before revoke %v, after %v", before, after)
	}

	roles.set(UserSub(1), "r:admin")
	memo := build(t, roles, grant("r:admin", "settings:manage"))
	memo.e.EnableGFunctionCache(true)
	before = enforce(t, memo, 1, "settings:manage")
	roles.set(UserSub(1))
	after = enforce(t, memo, 1, "settings:manage")
	t.Logf("memo on: before revoke %v, after revoke %v", before, after)
	if !after {
		t.Fatal("the memo no longer serves a stale link: revisit EnableGFunctionCache(false)")
	}
}

// The role manager answers from the source and refuses every write.
func TestRoleManagerReadOnly(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:a")
	m := newRoleManager(roles)
	if ok, _ := m.HasLink(UserSub(1), "r:a"); !ok {
		t.Fatal("HasLink")
	}
	if ok, _ := m.HasLink(UserSub(2), "r:a"); ok {
		t.Fatal("a cold user links")
	}
	if got, err := m.GetRoles("r:a"); err != nil || len(got) != 0 {
		t.Fatalf("roles of a role: %v %v", got, err)
	}
	if _, err := m.GetImplicitRoles(UserSub(2)); !errors.Is(err, ErrNotWarm) {
		t.Fatalf("cold user: %v", err)
	}
	for name, err := range map[string]error{
		"AddLink":    m.AddLink("u:1", "r:b"),
		"DeleteLink": m.DeleteLink("u:1", "r:a"),
	} {
		if !errors.Is(err, errReadOnly) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := m.GetUsers("r:a"); !errors.Is(err, errReadOnly) {
		t.Errorf("GetUsers: %v", err)
	}
	if err := m.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, _ := roles.Roles(UserSub(1)); !slices.Equal(got, []string{"r:a"}) {
		t.Fatal("Clear touched the source")
	}
}

// Readers of an immutable snapshot while another goroutine builds and
// swaps newer ones (run with -race); an older snapshot never replaces a
// newer one.
func TestConcurrentEnforceAndSwap(t *testing.T) {
	t.Parallel()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:t")
	eng := newEngine(nil, EngineOptions{})
	mk := func(v int64) *Snapshot {
		s, err := Build(Policy{Version: v, Active: map[string]bool{"account:read": true},
			Rules: []Rule{grant("r:t", "account:read:own")}}, Options{Roles: roles})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	eng.Swap(mk(1))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				s := eng.Current()
				if ok, err := s.Enforce(1, "account:read:own"); !ok || err != nil {
					t.Error("lost the grant", err)
					return
				}
				if set, err := s.Grants(1); err != nil || !set.Has("account:read:own") {
					t.Error("lost the grant", err)
					return
				}
			}
		})
	}
	for v := int64(2); v < 50; v++ {
		eng.Swap(mk(v))
	}
	wg.Wait()
	if eng.Swap(mk(10)) || eng.Current().Version() != 49 {
		t.Fatal("an older snapshot replaced a newer one")
	}
}
