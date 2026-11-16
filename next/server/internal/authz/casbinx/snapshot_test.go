package casbinx

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// core TestPermissionSetScoped through Casbin: Enforce and the set Grants
// returns answer each key as core's set does.
func TestPermissionSetScopedViaCasbin(t *testing.T) {
	t.Parallel()
	keys := []string{"account:read:own", "account:update:group/12", "proxy:read"}
	s := single(t, keys...)
	set := grants(t, s, held("r:t"))
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
		if got := enforce(t, s, held("r:t"), k); got != want {
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
	s := snapOf(t, Policy{Version: 3, Rules: []Rule{superRow("r:super_admin")}})
	su := held("r:super_admin")
	for _, k := range []string{"settings:manage", "plugin.newly_installed:x:manage:flow/3", "account:read:group/*"} {
		if !enforce(t, s, su, k) {
			t.Errorf("superuser lacks %s", k)
		}
	}
	if set := grants(t, s, su); !set.Superuser || set.Version != 3 || set.Keys == nil {
		t.Fatalf("Grants = %+v", set)
	}
	if enforce(t, s, held(), "settings:manage") {
		t.Fatal("a user without roles holds something")
	}
}

// Grants of actions that are not active (plugin disabled, permission
// removed) stay stored but are not loaded; rows of no catalog action, rows
// with an empty column and g rows are not loaded either, nor a stored row
// of SelfRole: every active user would hold it.
func TestInactivePermissionsNotLoaded(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		grant("r:t", "plugin.m:f:m:flow/2"), grant("r:t", "account:read"), grant("r:t", "proxy:read"),
		{"p", "r:t", "account:update", ""}, {"g", "u:1", "r:other", ""}, grant("r:other", "settings:manage"),
		grant(SelfRole, "settings:manage"),
	}
	active := map[string]bool{"account:read": true, "account:update": true, "settings:manage": true}
	s := snapOf(t, Policy{Version: 1, Active: active, Rules: rules})
	set := grants(t, s, held("r:t"))
	if got := set.KeyList(); !slices.Equal(got, []string{"account:read"}) {
		t.Fatalf("Grants = %v", got)
	}
	for k, want := range map[string]bool{"plugin.m:f:m:flow/2": false, "account:read:own": true, "proxy:read": false,
		"account:update:own": false, "settings:manage": false} {
		if got := enforce(t, s, held("r:t"), k); got != want {
			t.Errorf("Enforce(%s) = %v, want %v", k, got, want)
		}
	}
}

// The self permissions are SelfRole's: held by every active user, by no
// inactive one (whatever roles it names).
func TestSelfPermissions(t *testing.T) {
	t.Parallel()
	self := []string{"self:profile", "self:security"}
	s := snapOf(t, Policy{Version: 1, Rules: []Rule{grant("r:t", "proxy:read")}, Active: map[string]bool{"proxy:read": true}}, self...)
	if got := grants(t, s, held("r:t")).KeyList(); !slices.Equal(got, []string{"proxy:read", "self:profile", "self:security"}) {
		t.Fatalf("active user: %v", got)
	}
	inactive := UserRoles{Roles: []string{"r:t"}}
	if set := grants(t, s, inactive); len(set.KeyList()) != 0 || set.Superuser || set.Keys == nil {
		t.Fatalf("inactive user: %+v", set)
	}
	if enforce(t, s, inactive, "self:profile") || !enforce(t, s, held(), "self:profile") {
		t.Fatal("self permission held by the wrong user")
	}
	if _, err := build(Policy{}, []string{"self:x:own"}); err == nil {
		t.Fatal("an invalid self key was taken")
	}
}

// authz TestCanGrant / core Missing on Casbin-built sets: the
// anti-escalation comparison stays ours, fed by Grants.
func TestCanGrantOnCasbinGrants(t *testing.T) {
	t.Parallel()
	s := snap(t, grant("r:a", "user:read"), grant("r:a", "user:update"), superRow("r:super_admin"), grant("r:b", "user:read"))
	for _, tc := range []struct {
		actor string
		perms []string
		deny  bool
	}{
		{"r:super_admin", []string{"user:read", "user:update", "settings:manage"}, false},
		{"r:a", []string{"user:read"}, false},
		{"r:a", []string{"user:read", "user:update"}, false},
		{"r:b", []string{"user:update"}, true},
		{"r:b", []string{"user:read", "user:update"}, true},
		{"r:a", nil, false},
	} {
		m := grants(t, s, held(tc.actor)).Missing(core.PermissionSetOf(tc.perms...))
		if !m.Empty() != tc.deny {
			t.Errorf("actor %s grants %v: missing %+v", tc.actor, tc.perms, m)
		}
	}
}

// core TestRestrictScoped on a Casbin-built set: Restrict (access keys)
// stays ours and answers the same.
func TestRestrictScopedOnCasbinGrants(t *testing.T) {
	t.Parallel()
	user := grants(t, single(t, "account:read:own", "account:update:group/12", "account:delete", "proxy:read:own"), held("r:t"))
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
	userOK := enforce(t, s, held("r:t"), want)
	keyOK := permMatch("account:update", "group/12", want)
	restricted := grants(t, s, held("r:t")).Restrict([]string{"account:update:group/12"}).Has(want)
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
		if got := enforce(t, s, held("r:t"), k); got != want {
			t.Errorf("Enforce(%s) = %v", k, got)
		}
	}
	if got := grants(t, s, held("r:t")).KeyList(); !slices.Equal(got, []string{a + ":flow/2"}) {
		t.Fatalf("Grants = %v", got)
	}
}

// Equivalence with core's sets, offline: random roles over the vector keys
// and random users. For every key the matcher must answer as the set built
// directly from the user's roles (core's PermissionSet.Has), and Grants
// must hold the same keys and superuser flag.
func TestCasbinEquivalenceRandom(t *testing.T) {
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
			role := casbinrule.RoleSub(string(rune('a' + ri)))
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
		users := map[int64]UserRoles{}
		want := map[int64]core.PermissionSet{}
		for uid := int64(1); uid <= 5; uid++ {
			set := core.PermissionSet{Keys: map[string]struct{}{}}
			active := r.IntN(8) != 0
			var roles []string
			for _, role := range []string{"r:a", "r:b", "r:c", "r:d", "r:su"} {
				if !active || r.IntN(3) != 0 {
					continue
				}
				roles = append(roles, role)
				set.Superuser = set.Superuser || (role == "r:su" && superRole)
				for _, k := range roleKeys[role] {
					set.Add(k)
				}
			}
			if active {
				for _, k := range self {
					set.Add(k)
				}
			}
			users[uid] = UserRoles{Version: 1, Active: active, Roles: roles}
			want[uid] = set
		}
		s := snapOf(t, Policy{Version: 1, Active: activeOf(rules), Rules: rules}, self...)
		for uid, set := range want {
			for _, q := range queries {
				if got := enforce(t, s, users[uid], q); got != set.Has(q) {
					t.Fatalf("user %d %q: casbin %v, core %v", uid, q, got, set.Has(q))
				}
				checks++
			}
			g := grants(t, s, users[uid])
			if g.Superuser != set.Superuser || !slices.Equal(g.KeyList(), set.KeyList()) {
				t.Fatalf("user %d grants %v (super %v), core %v (super %v)", uid, g.KeyList(), g.Superuser, set.KeyList(), set.Superuser)
			}
		}
	}
	t.Logf("%d comparisons, 0 mismatches", checks)
}

// A user's links are the roles each call brings (read by RoleCache at the
// snapshot's version), not something the enforcer remembers: a revoked role
// stops at the next call, Casbin's g memo or not, and a user subject asked
// directly links to nothing (fail closed).
func TestRolesComeWithEachCall(t *testing.T) {
	t.Parallel()
	for _, memo := range []bool{false, true} {
		s := snap(t, grant("r:admin", "settings:manage"))
		s.e.EnableGFunctionCache(memo)
		if !enforce(t, s, held("r:admin"), "settings:manage") || enforce(t, s, held(), "settings:manage") {
			t.Fatalf("memo %v: the roles of the call are not what decides", memo)
		}
		if ok, err := s.e.Enforce(casbinrule.UserSub(1), "settings:manage"); ok || err != nil {
			t.Fatalf("memo %v: a user subject links to a role: %v, %v", memo, ok, err)
		}
	}
}

// The role manager links a subject to itself only and refuses every write.
func TestRoleManagerReadOnly(t *testing.T) {
	t.Parallel()
	m := roleManager{}
	if ok, _ := m.HasLink("r:a", "r:a"); !ok {
		t.Fatal("HasLink of a role to itself")
	}
	if ok, _ := m.HasLink(casbinrule.UserSub(1), "r:a"); ok {
		t.Fatal("a user links to a role")
	}
	if got, err := m.GetImplicitRoles("r:a"); err != nil || len(got) != 0 {
		t.Fatalf("roles of a role: %v %v", got, err)
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
}

// Readers of an immutable snapshot while another goroutine builds and
// swaps newer ones (run with -race); an older snapshot never replaces a
// newer one.
func TestConcurrentEnforceAndSwap(t *testing.T) {
	t.Parallel()
	eng := newSnapshots(nil, snapshotOptions{})
	mk := func(v int64) *snapshot {
		return snapOf(t, Policy{Version: v, Active: map[string]bool{"account:read": true},
			Rules: []Rule{grant("r:t", "account:read:own")}})
	}
	eng.swap(mk(1))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				s := eng.cur.Load()
				if ok, err := s.enforce(held("r:t"), "account:read:own"); !ok || err != nil {
					t.Error("lost the grant", err)
					return
				}
				if set, err := s.grants(held("r:t")); err != nil || !set.Has("account:read:own") {
					t.Error("lost the grant", err)
					return
				}
			}
		})
	}
	for v := int64(2); v < 50; v++ {
		eng.swap(mk(v))
	}
	wg.Wait()
	if eng.swap(mk(10)) || eng.cur.Load().version != 49 {
		t.Fatal("an older snapshot replaced a newer one")
	}
}
