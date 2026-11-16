package casbinx

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// core TestPermissionSetScoped through Casbin: Enforce, and the lists
// Permissions returns, answer each key as core's set does.
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
		if got := listsOf(set).Has(k); got != want {
			t.Errorf("Permissions lists %s = %v, want %v", k, got, want)
		}
		if got := set.Has(k); got != want {
			t.Errorf("Permissions.Has(%s) = %v, want %v", k, got, want)
		}
	}
	if !slices.Equal(set.KeyList(), core.PermissionSetOf(keys...).KeyList()) {
		t.Fatalf("KeyList = %v", set.KeyList())
	}
}

// A superuser row holds every key, those added after the policy was loaded
// included (a plugin installed later), as core's Superuser flag does; it
// loads whatever the catalog says.
func TestSuperuserFutureKeys(t *testing.T) {
	t.Parallel()
	s := snapOf(t, Policy{Version: 3, Rules: []Rule{superRow("r:super_admin")}})
	su := UserRoles{Version: 3, Active: true, Roles: []string{"r:super_admin"}}
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
// with an empty column and the g rows of users not active are not loaded
// either, nor a stored row of SelfRole: every active user would hold it.
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

// authz TestCanGrant / core Missing on Casbin's sets: the comparison is
// core's, each key of it decided by Enforce.
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

// core TestRestrictScoped on Casbin's set: Restrict (access keys) narrows
// Casbin's lists and answers the same.
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

// Why an access key is not a matcher "user may AND key allows": the user
// holds account/5, the access key names group/12; both imply
// group/12/account/5, but Restrict drops scopes neither implies rather than
// intersecting them, so the key reaches nothing there. A narrowed set's Has
// is the narrowed lists AND Enforce: the decision stays Casbin's, and never
// reaches past what the key's DataScope lists.
func TestRestrictIsNotAMatcherIntersection(t *testing.T) {
	t.Parallel()
	s := single(t, "account:update:account/5")
	want := "account:update:group/12/account/5"
	userOK := enforce(t, s, held("r:t"), want)
	keyOK := new(matcher).permMatch("account:update", "group/12", want)
	restricted := grants(t, s, held("r:t")).Restrict([]string{"account:update:group/12"}).Has(want)
	if !grants(t, s, held("r:t")).Has(want) {
		t.Fatal("the user's own set lost the key")
	}
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
// and random users, loaded as a node loads them (p and g rows, the active
// users; an inactive one read when first asked about). For every key
// Casbin's Enforce must answer as the set built directly from the user's
// roles (core's PermissionSet.Has), and Permissions must list the same keys
// and superuser flag.
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
	ctx := context.Background()
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
		l := &fakeLoader{users: map[int64]UserRoles{}}
		want := map[int64]core.PermissionSet{}
		for uid := int64(1); uid <= 5; uid++ {
			set := core.PermissionSet{Keys: map[string]struct{}{}}
			active := r.IntN(8) != 0
			var roles []string
			for _, role := range []string{"r:a", "r:b", "r:c", "r:d", "r:su"} {
				if r.IntN(3) != 0 {
					continue
				}
				roles = append(roles, role)
				rules = append(rules, Rule{"g", casbinrule.UserSub(uid), role, ""})
				if !active {
					continue
				}
				set.Superuser = set.Superuser || (role == "r:su" && superRole)
				for _, k := range roleKeys[role] {
					set.Add(k)
				}
			}
			if active {
				for _, k := range self {
					set.Add(k)
				}
				l.policy.Users = append(l.policy.Users, uid)
			} else {
				l.users[uid] = UserRoles{Version: 1}
			}
			want[uid] = set
		}
		l.policy = Policy{Version: 1, Active: activeOf(rules), Rules: rules, Users: l.policy.Users}
		e := newEngine(l, self, Options{})
		for uid, set := range want {
			for _, q := range queries {
				got, err := e.Enforce(ctx, uid, 1, q)
				if err != nil {
					t.Fatal(err)
				}
				if got != set.Has(q) {
					t.Fatalf("user %d %q: casbin %v, core %v", uid, q, got, set.Has(q))
				}
				checks++
			}
			g, err := e.Permissions(ctx, uid, 1)
			if err != nil {
				t.Fatal(err)
			}
			if g.Superuser != set.Superuser || !slices.Equal(g.KeyList(), set.KeyList()) {
				t.Fatalf("user %d lists %v (super %v), core %v (super %v)", uid, g.KeyList(), g.Superuser, set.KeyList(), set.Superuser)
			}
		}
	}
	t.Logf("%d comparisons, 0 mismatches", checks)
}

// Readers deciding while a user's roles change back and forth and newer
// states replace the current one (run with -race): each decision is one a
// state held, a user's change is never undone by a decision made before
// it, and an older state never replaces a newer one.
func TestConcurrentEnforceAndChange(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{policy: Policy{Version: 1, PolicyVersion: 1, Active: map[string]bool{"account:read": true, "proxy:read": true},
		Rules: []Rule{grant("r:t", "account:read:own"), grant("r:p", "proxy:read"), {"g", "u:1", "r:t", ""}, {"g", "u:2", "r:t", ""}},
		Users: []int64{1, 2}}}
	e := newEngine(l, nil, Options{})
	ctx := context.Background()
	if _, err := e.Enforce(ctx, 1, 1, "account:read:own"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if ok, err := e.Enforce(ctx, 1, 0, "account:read:own"); !ok || err != nil {
					t.Error("user 1 lost its grant", err)
					return
				}
				if _, err := e.Enforce(ctx, 2, 0, "proxy:read"); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	st := e.cur.Load()
	for i := range 200 {
		roles := []string{"r:t"}
		if i%2 == 0 {
			roles = append(roles, "r:p")
		}
		if err := st.setUser(2, UserRoles{Version: int64(i + 2), PolicyVersion: 1, Active: true, Roles: roles}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	// The last change (i = 199) took r:p away: no decision made before it
	// may answer after it.
	if ok, err := e.Enforce(ctx, 2, 0, "proxy:read"); ok || err != nil {
		t.Fatalf("a decision older than user 2's last change: %v %v", ok, err)
	}
}
