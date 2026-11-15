package casbinx

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/permvectors"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// mapRoles is a RoleSource over a map, standing in for RoleCache's L1.
type mapRoles struct {
	mu sync.RWMutex
	m  map[string][]string
}

func newMapRoles() *mapRoles { return &mapRoles{m: map[string][]string{}} }

// set gives sub roles (none: an entry without roles, as an inactive user's).
func (r *mapRoles) set(sub string, roles ...string) {
	r.mu.Lock()
	r.m[sub] = roles
	r.mu.Unlock()
}

func (r *mapRoles) drop(sub string) {
	r.mu.Lock()
	delete(r.m, sub)
	r.mu.Unlock()
}

func (r *mapRoles) Roles(sub string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	roles, ok := r.m[sub]
	return roles, ok
}

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

// build is Build at version 1 with every action active.
func build(t testing.TB, roles RoleSource, rules ...Rule) *Snapshot {
	t.Helper()
	s, err := Build(Policy{Version: 1, Active: activeOf(rules), Rules: rules}, Options{Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// single is the snapshot where user 1 holds one role, r:t, granting keys.
func single(t testing.TB, keys ...string) *Snapshot {
	t.Helper()
	roles := newMapRoles()
	roles.set(UserSub(1), "r:t")
	var rules []Rule
	for _, k := range keys {
		rules = append(rules, grant("r:t", k))
	}
	return build(t, roles, rules...)
}

func grants(t testing.TB, s *Snapshot, uid int64) core.PermissionSet {
	t.Helper()
	set, err := s.Grants(uid)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func enforce(t testing.TB, s *Snapshot, uid int64, key string) bool {
	t.Helper()
	ok, err := s.Enforce(uid, key)
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
		if got := enforce(t, s, 1, c.Want); got != c.OK {
			t.Errorf("Enforce(have %q, want %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
		if got := grants(t, s, 1).Has(c.Want); got != c.OK {
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
		if got := PermMatch(string(have.Action), scopeOf(have), c.Want); got != c.OK {
			t.Errorf("PermMatch(%q, %q) = %v, want %v", c.Have, c.Want, got, c.OK)
		}
	}
	if !PermMatch(SuperKey, AllRows, "plugin.later:x:y:flow/3") {
		t.Fatal("SuperKey does not match a future key")
	}
	if PermMatch("account:read", AllRows, "account:read:bogus/") {
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
		s := grants(t, single(t, c.Have...), 1).DataScope(c.Me, perm.Key(c.Action))
		if got := s.AllowsRow(c.Row.ID, c.Row.CreatedBy, c.Row.GroupIDs); got != c.OK {
			t.Errorf("%v %s by %d on %+v: %v, want %v", c.Have, c.Action, c.Me, c.Row, got, c.OK)
		}
	}
}
