package casbinx

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
)

// benchPolicy is a production-sized policy: 10 roles of 40 grants each
// (core actions, scoped account and proxy keys, plugin keys) and a
// superuser role, 401 p rows; benchUser holds role0 and one more role (and
// SelfRole, as active).
func benchPolicy() Policy {
	var rules []Rule
	for r := range 10 {
		role := casbinrule.RoleSub(fmt.Sprintf("role%d", r))
		for k := range 40 {
			var key string
			switch k % 4 {
			case 0:
				key = fmt.Sprintf("mod%d:act%d", k, r)
			case 1:
				key = fmt.Sprintf("account:read:group/%d", k*10+r+1)
			case 2:
				key = fmt.Sprintf("plugin.p%d:f:act%d:flow/%d", r, k, k+1)
			default:
				key = fmt.Sprintf("proxy:read:proxy/%d", k*10+r+1)
			}
			rules = append(rules, grant(role, key))
		}
	}
	rules = append(rules, superRow("r:super_admin"))
	return Policy{Version: 1, Active: activeOf(rules), Rules: rules}
}

var (
	benchSelf = []string{"self:profile", "self:security", "self:plugin_ui"}
	benchUser = held("r:role0", "r:role1")
)

func BenchmarkBuild400p(b *testing.B) {
	p := benchPolicy()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := build(p, benchSelf); err != nil {
			b.Fatal(err)
		}
	}
}

// Grants is the cost of a PermissionSet cache miss (once per user and
// version).
func BenchmarkGrants(b *testing.B) {
	s, err := build(benchPolicy(), benchSelf)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.grants(benchUser); err != nil {
			b.Fatal(err)
		}
	}
}

// EnsureHit is the per-miss check that the snapshot is current.
func BenchmarkEnsureHit(b *testing.B) {
	s, err := build(benchPolicy(), nil)
	if err != nil {
		b.Fatal(err)
	}
	eng := newSnapshots(nil, snapshotOptions{})
	eng.swap(s)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := eng.Ensure(ctx, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// Enforce against Has on the same set: why hot paths use the set.
func BenchmarkEnforceVsHas(b *testing.B) {
	s, err := build(benchPolicy(), benchSelf)
	if err != nil {
		b.Fatal(err)
	}
	set, err := s.grants(benchUser)
	if err != nil {
		b.Fatal(err)
	}
	for _, q := range []struct{ name, key string }{
		{"hit", "mod0:act0"},
		{"hit_scoped", "account:read:group/12/account/7"},
		{"miss", "settings:manage"},
	} {
		b.Run("Enforce/"+q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.enforce(benchUser, q.key); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("Has/"+q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = set.Has(q.key)
			}
		})
	}
}
