package casbinx

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
)

// benchPolicy is a production-sized policy: 10 roles of grants grants
// each (core actions, scoped account and proxy keys, plugin keys) and a
// superuser role.
func benchPolicy(grants int) Policy {
	var rules []Rule
	roles := map[string]int64{"super_admin": 11}
	for r := range 10 {
		key := fmt.Sprintf("role%d", r)
		roles[key] = int64(r + 1)
		for k := range grants {
			var g string
			switch k % 4 {
			case 0:
				g = fmt.Sprintf("mod%d:act%d", k, r)
			case 1:
				g = fmt.Sprintf("account:read:group/%d", k*10+r+1)
			case 2:
				g = fmt.Sprintf("plugin.p%d:f:act%d:flow/%d", r, k, k+1)
			default:
				g = fmt.Sprintf("proxy:read:proxy/%d", k*10+r+1)
			}
			rules = append(rules, grant(casbinrule.RoleSub(key), g))
		}
	}
	rules = append(rules, superRow("r:super_admin"))
	return Policy{Version: 1, PolicyVersion: 1, Active: activeOf(rules), Rules: rules, Roles: roles}
}

// benchUser is user uid as read: active, holding role uid%10 and role
// (uid+3)%10.
func benchUser(uid int64) UserRoles {
	return UserRoles{Version: 1, PolicyVersion: 1, Active: true,
		Roles: []string{casbinrule.RoleSub(fmt.Sprintf("role%d", uid%10)), casbinrule.RoleSub(fmt.Sprintf("role%d", (uid+3)%10))}}
}

var benchSelf = []string{"self:profile", "self:security", "self:plugin_ui"}

// benchSizes are the policies measured: grants per role, users known.
var benchSizes = []struct{ grants, users int }{{40, 1000}, {200, 1000}, {40, 10000}, {200, 10000}, {40, 100000}}

// benchState is the state of benchPolicy(grants) knowing users users,
// 1..users, as a node knows those asked about.
func benchState(b *testing.B, grants, users int) *state {
	b.Helper()
	st, err := build(benchPolicy(grants), benchSelf, users)
	if err != nil {
		b.Fatal(err)
	}
	for uid := int64(1); uid <= int64(users); uid++ {
		if ok, err := st.setUser(uid, benchUser(uid)); !ok || err != nil {
			b.Fatal("not set", err)
		}
	}
	return st
}

func benchEngine(b *testing.B, grants, users int) *engine {
	return engineOf(benchState(b, grants, users))
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
						st.lists.Delete(int64(1))
					}
					if _, err := e.Permissions(ctx, 1, 1); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Load: a new enforcer from a policy read, holding no user (a policy write
// of unknown extent, on every node), and the reload of the p rows in place,
// keeping the users known (a policy write, on every node).
func BenchmarkLoad(b *testing.B) {
	for _, grants := range []int{40, 200} {
		p := benchPolicy(grants)
		b.Run(fmt.Sprintf("build/g%d", grants), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := build(p, benchSelf, 1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	for _, sz := range benchSizes {
		p, st := benchPolicy(sz.grants), benchState(b, sz.grants, sz.users)
		b.Run(fmt.Sprintf("reload/g%d/u%d", sz.grants, sz.users), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p.Version++
				if err := st.reload(p, benchSelf, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Users: a user read in (learnt when first asked about), and the memory
// the users known take.
func BenchmarkUsers(b *testing.B) {
	for _, sz := range benchSizes {
		b.Run(fmt.Sprintf("learn/g%d/u%d", sz.grants, sz.users), func(b *testing.B) {
			st := benchState(b, sz.grants, sz.users)
			b.ReportAllocs()
			uid := int64(sz.users)
			for b.Loop() {
				uid++ // past capacity: one known user is forgotten for each
				if ok, err := st.setUser(uid, benchUser(uid)); !ok || err != nil {
					b.Fatal("not set", err)
				}
			}
		})
	}
	for _, users := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("memory/u%d", users), func(b *testing.B) {
			for b.Loop() {
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				st := benchState(b, 40, users)
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/float64(users), "B/user")
				runtime.KeepAlive(st)
			}
		})
	}
}

// UserChange: one user's g rows replaced in memory (a user write, on every
// node).
func BenchmarkUserChange(b *testing.B) {
	for _, sz := range benchSizes {
		st := benchState(b, sz.grants, sz.users)
		b.Run(fmt.Sprintf("g%d/u%d", sz.grants, sz.users), func(b *testing.B) {
			b.ReportAllocs()
			v := int64(1)
			for b.Loop() {
				v++
				ur := benchUser(1)
				ur.Version = v
				if ok, err := st.setUser(1, ur); !ok || err != nil {
					b.Fatal("not set", err)
				}
			}
		})
	}
}
