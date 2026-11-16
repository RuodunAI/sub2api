package casbinx

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/permvectors"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// grant is the p row of role (a subject) granting key, in any spelling.
func grant(role, key string) Rule {
	p, err := perm.Parse(key)
	if err != nil {
		return Rule{"p", role, key, AllRows} // stored as is: matches nothing
	}
	return Rule{"p", role, string(p.Action), scopeOf(p)}
}

func superRow(role string) Rule { return Rule{"p", role, SuperKey, AllRows} }

// activeOf marks the action of every p row active.
func activeOf(rules []Rule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		out[r.V1] = true
	}
	return out
}

// testUser is the one user the single-user helpers ask about.
const testUser int64 = 1

// held is the g rows of an active user testUser holding roles.
func held(roles ...string) UserRoles { return UserRoles{Version: 1, Active: true, Roles: roles} }

// snap is a state at version 1 with every action active, holding no user.
func snap(t testing.TB, rules ...Rule) *state {
	t.Helper()
	return snapOf(t, Policy{Version: 1, Active: activeOf(rules), Rules: rules})
}

func snapOf(t testing.TB, p Policy, self ...string) *state {
	t.Helper()
	s, err := build(p, self, Options{}.withDefaults().Users)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// single is the state where role r:t grants keys (user: held("r:t")).
func single(t testing.TB, keys ...string) *state {
	t.Helper()
	var rules []Rule
	for _, k := range keys {
		rules = append(rules, grant("r:t", k))
	}
	return snap(t, rules...)
}

// as sets user uid's g rows in s to ur, whatever was set before.
func (s *state) as(t testing.TB, uid int64, ur UserRoles) *state {
	t.Helper()
	ur.Version, ur.PolicyVersion = math.MaxInt64, s.policy
	if ok, err := s.setUser(uid, ur); !ok || err != nil {
		t.Fatal("not set", err)
	}
	return s
}

// engineOf is an engine whose current state is s (nothing to load).
func engineOf(s *state) *engine {
	e := newEngine(nil, nil, Options{})
	e.cur.Store(s)
	return e
}

// grants is Permissions for testUser holding ur in s.
func grants(t testing.TB, s *state, ur UserRoles) core.PermissionSet {
	t.Helper()
	set, err := engineOf(s.as(t, testUser, ur)).Permissions(context.Background(), testUser, 0)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// enforce is Casbin's Enforce of key for testUser holding ur in s.
func enforce(t testing.TB, s *state, ur UserRoles, key string) bool {
	t.Helper()
	ok, err := engineOf(s.as(t, testUser, ur)).Enforce(context.Background(), testUser, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// listsOf is set without its decisions: Has then answers from the lists
// Casbin returned, which the tests compare with Enforce.
func listsOf(set core.PermissionSet) core.PermissionSet { return set.Decided(nil) }

type vectors struct {
	Implies []struct {
		Have, Want string
		OK         bool
	}
	Allows []struct {
		Have   []string
		Action string
		Me     int64
		Row    struct {
			ID        int64   `json:"id"`
			CreatedBy *int64  `json:"created_by"`
			GroupIDs  []int64 `json:"group_ids"`
		}
		OK bool
	}
}

func loadVectors(t testing.TB) vectors {
	t.Helper()
	var v vectors
	if err := json.Unmarshal(permvectors.JSON, &v); err != nil || len(v.Implies) == 0 || len(v.Allows) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	return v
}

// The shared implies vectors (sdk/permvectors, perm TestImpliesVectors)
// through Casbin: have granted to a role, want enforced for its holder and
// tested on the lists Permissions returns. Both answer as perm.Implies.
func TestImpliesVectorsViaCasbin(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	for _, c := range v.Implies {
		s := single(t, c.Have)
		if got := enforce(t, s, held("r:t"), c.Want); got != c.OK {
			t.Errorf("Enforce(have %q, want %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
		if got := listsOf(grants(t, s, held("r:t"))).Has(c.Want); got != c.OK {
			t.Errorf("Permissions(have %q) lists %q = %v, want %v", c.Have, c.Want, got, c.OK)
		}
	}
	t.Logf("%d implies vectors via Casbin", len(v.Implies))
}

// permMatch is perm.Implies: on the vectors, called the way the matcher
// calls it (the columns of the stored grant).
func TestPermMatchIsImplies(t *testing.T) {
	t.Parallel()
	m := new(matcher)
	for _, c := range loadVectors(t).Implies {
		have, err := perm.Parse(c.Have)
		if err != nil {
			t.Fatal(c.Have, err)
		}
		if got := m.permMatch(string(have.Action), scopeOf(have), c.Want); got != c.OK {
			t.Errorf("permMatch(%q, %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
	}
	if !m.permMatch(SuperKey, AllRows, "plugin.later:x:y:flow/3") {
		t.Fatal("SuperKey does not match a future key")
	}
	if m.permMatch("account:read", AllRows, "account:read:bogus/") {
		t.Fatal("an unparsable key matched")
	}
	if _, err := m.fn("a", "b"); err == nil {
		t.Fatal("permMatch took 2 arguments")
	}
}

// The parse cache stops growing at its limit and keeps answering.
func TestParseCacheBounded(t *testing.T) {
	m := new(matcher)
	for i := range parseCacheLimit + 100 {
		if _, ok := m.parsed("account:read:group/" + strconv.Itoa(i+1)); !ok {
			t.Fatal("parse failed")
		}
	}
	if n := m.size.Load(); n > parseCacheLimit {
		t.Fatalf("cache holds %d keys, limit %d", n, parseCacheLimit)
	}
	if p, ok := m.parsed("account:read:group/999999"); !ok || p.Scope.Group != 999999 {
		t.Fatalf("past the limit: %+v %v", p, ok)
	}
}

// The shared row-check vectors (core TestAllowsRowVectors) on the set
// Permissions returns: DataScope stays ours, fed by Casbin's lists.
func TestAllowsRowVectorsOnCasbinGrants(t *testing.T) {
	t.Parallel()
	for _, c := range loadVectors(t).Allows {
		s := grants(t, single(t, c.Have...), held("r:t")).DataScope(c.Me, perm.Key(c.Action))
		if got := s.AllowsRow(c.Row.ID, c.Row.CreatedBy, c.Row.GroupIDs); got != c.OK {
			t.Errorf("%v %s by %d on %+v: %v, want %v", c.Have, c.Action, c.Me, c.Row, got, c.OK)
		}
	}
}
