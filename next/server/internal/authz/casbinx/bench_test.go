package casbinx

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
)

// benchPolicy is a production-sized policy: 10 roles of grants grants
// each (core actions, scoped account and proxy keys, plugin keys), a
// superuser role, and users active users each holding two roles (user i:
// role i%10 and role (i+3)%10).
func benchPolicy(grants, users int) Policy {
	var rules []Rule
	for r := range 10 {
		role := casbinrule.RoleSub(fmt.Sprintf("role%d", r))
		for k := range grants {
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
	ids := make([]int64, users)
	for i := range users {
		uid := int64(i + 1)
		ids[i] = uid
		for _, r := range []int{i % 10, (i + 3) % 10} {
			rules = append(rules, Rule{"g", casbinrule.UserSub(uid), casbinrule.RoleSub(fmt.Sprintf("role%d", r)), ""})
		}
	}
	return Policy{Version: 1, PolicyVersion: 1, Active: activeOf(rules), Rules: rules, Users: ids}
}

var benchSelf = []string{"self:profile", "self:security", "self:plugin_ui"}

// benchSizes are the policies measured: grants per role, active users.
var benchSizes = []struct{ grants, users int }{{40, 1000}, {200, 1000}, {40, 10000}, {200, 10000}}

func benchEngine(b *testing.B, grants, users int) *engine {
	b.Helper()
	st, err := build(benchPolicy(grants, users), benchSelf)
	if err != nil {
		b.Fatal(err)
	}
	return engineOf(st)
}

// The keys user 1 (role0, role3) is asked about: held, scoped (role0
// holds account:read:group/2, so group/2/account/7 is in it), and not held.
const (
	benchHeld   = "mod0:act0"
	benchScoped = "account:read:group/2/account/7"
	benchDenied = "settings:manage"
)

// Enforce through the decision cache: a hit (allow, deny, scoped), and a
// first-time decision (the user's cached decisions dropped before each:
// Casbin's matcher over every p row).
func BenchmarkEnforce(b *testing.B) {
	ctx := context.Background()
	for _, sz := range benchSizes {
		e := benchEngine(b, sz.grants, sz.users)
		st := e.cur.Load()
		for _, q := range []struct {
			name, key string
			miss      bool
		}{
			{"hit", benchHeld, false},
			{"hit_deny", benchDenied, false},
			{"hit_scoped", benchScoped, false},
			{"miss", benchHeld, true},
			{"miss_deny", benchDenied, true},
			{"miss_scoped", benchScoped, true},
		} {
			b.Run(fmt.Sprintf("g%d/u%d/%s", sz.grants, sz.users, q.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if q.miss {
						st.dc.dropSubject("u:1")
					}
					if _, err := e.Enforce(ctx, 1, 1, q.key); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		b.Run(fmt.Sprintf("g%d/u%d/hit_parallel", sz.grants, sz.users), func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := e.Enforce(ctx, 1, 1, benchHeld); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// Permissions: the lists of a user's grants (GetImplicitPermissionsForUser),
// cached and first-time.
func BenchmarkPermissions(b *testing.B) {
	ctx := context.Background()
	for _, sz := range benchSizes {
		e := benchEngine(b, sz.grants, sz.users)
		st := e.cur.Load()
		for _, miss := range []bool{false, true} {
			b.Run(fmt.Sprintf("g%d/u%d/miss=%v", sz.grants, sz.users, miss), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if miss {
						if _, ok := st.lists.LoadAndDelete(int64(1)); ok {
							st.nLists.Add(-1)
						}
					}
					if _, err := e.Permissions(ctx, 1, 1); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Load: a new enforcer from a policy read (a policy write, on every node).
func BenchmarkLoad(b *testing.B) {
	for _, sz := range benchSizes {
		p := benchPolicy(sz.grants, sz.users)
		b.Run(fmt.Sprintf("g%d/u%d", sz.grants, sz.users), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := build(p, benchSelf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// UserChange: one user's g rows replaced in memory (a user write, on every
// node).
func BenchmarkUserChange(b *testing.B) {
	for _, sz := range benchSizes {
		st := benchEngine(b, sz.grants, sz.users).cur.Load()
		b.Run(fmt.Sprintf("g%d/u%d", sz.grants, sz.users), func(b *testing.B) {
			b.ReportAllocs()
			v := int64(1)
			for b.Loop() {
				v++
				if err := st.setUser(1, UserRoles{Version: v, PolicyVersion: 1, Active: true, Roles: []string{"r:role0", "r:role3"}}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
