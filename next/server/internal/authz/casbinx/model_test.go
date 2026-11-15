package casbinx

import (
	"encoding/json"
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

// held is the roles of an active user holding roles.
func held(roles ...string) UserRoles { return UserRoles{Active: true, Roles: roles} }

// snap is build at version 1 with every action active.
func snap(t testing.TB, rules ...Rule) *snapshot {
	t.Helper()
	return snapOf(t, Policy{Version: 1, Active: activeOf(rules), Rules: rules})
}

func snapOf(t testing.TB, p Policy, self ...string) *snapshot {
	t.Helper()
	s, err := build(p, self)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// single is the snapshot where role r:t grants keys (user: held("r:t")).
func single(t testing.TB, keys ...string) *snapshot {
	t.Helper()
	var rules []Rule
	for _, k := range keys {
		rules = append(rules, grant("r:t", k))
	}
	return snap(t, rules...)
}

func grants(t testing.TB, s *snapshot, ur UserRoles) core.PermissionSet {
	t.Helper()
	set, err := s.grants(ur)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func enforce(t testing.TB, s *snapshot, ur UserRoles, key string) bool {
	t.Helper()
	ok, err := s.enforce(ur, key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

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
// tested on the set Grants returns. Both answer as perm.Implies.
func TestImpliesVectorsViaCasbin(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	for _, c := range v.Implies {
		s := single(t, c.Have)
		if got := enforce(t, s, held("r:t"), c.Want); got != c.OK {
			t.Errorf("Enforce(have %q, want %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
		if got := grants(t, s, held("r:t")).Has(c.Want); got != c.OK {
			t.Errorf("Grants(have %q).Has(%q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
	}
	t.Logf("%d implies vectors via Casbin", len(v.Implies))
}

// permMatch is perm.Implies: on the vectors, called the way the matcher
// calls it (the columns of the stored grant).
func TestPermMatchIsImplies(t *testing.T) {
	t.Parallel()
	for _, c := range loadVectors(t).Implies {
		have, err := perm.Parse(c.Have)
		if err != nil {
			t.Fatal(c.Have, err)
		}
		if got := permMatch(string(have.Action), scopeOf(have), c.Want); got != c.OK {
			t.Errorf("permMatch(%q, %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
	}
	if !permMatch(SuperKey, AllRows, "plugin.later:x:y:flow/3") {
		t.Fatal("SuperKey does not match a future key")
	}
	if permMatch("account:read", AllRows, "account:read:bogus/") {
		t.Fatal("an unparsable key matched")
	}
	if _, err := permMatchFunc("a", "b"); err == nil {
		t.Fatal("permMatch took 2 arguments")
	}
}

// The parse cache stops growing at its limit and keeps answering.
func TestParseCacheBounded(t *testing.T) {
	for i := range parseCacheLimit + 100 {
		if _, ok := parsed("account:read:group/" + strconv.Itoa(i+1)); !ok {
			t.Fatal("parse failed")
		}
	}
	if n := parseCacheSize.Load(); n > parseCacheLimit {
		t.Fatalf("cache holds %d keys, limit %d", n, parseCacheLimit)
	}
	if p, ok := parsed("account:read:group/999999"); !ok || p.Scope.Group != 999999 {
		t.Fatalf("past the limit: %+v %v", p, ok)
	}
}

// The shared row-check vectors (core TestAllowsRowVectors) on the set
// Grants returns: DataScope stays ours, fed by Casbin's set.
func TestAllowsRowVectorsOnCasbinGrants(t *testing.T) {
	t.Parallel()
	for _, c := range loadVectors(t).Allows {
		s := grants(t, single(t, c.Have...), held("r:t")).DataScope(c.Me, perm.Key(c.Action))
		if got := s.AllowsRow(c.Row.ID, c.Row.CreatedBy, c.Row.GroupIDs); got != c.OK {
			t.Errorf("%v %s by %d on %+v: %v, want %v", c.Have, c.Action, c.Me, c.Row, got, c.OK)
		}
	}
}
