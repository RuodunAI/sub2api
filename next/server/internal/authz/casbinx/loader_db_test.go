package casbinx_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// policyDB is a migrated database, skipping the test while casbin_rule is
// not in the migrations yet (migration 0100 lands separately). The tables
// are the migration's: the tests write the legacy tables only and read
// casbin_rule as the triggers project it.
func policyDB(t *testing.T) *store.DB {
	t.Helper()
	db := testutil.DB(t)
	var exists bool
	if err := db.SQL.QueryRow(`SELECT to_regclass('casbin_rule') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("casbin_rule does not exist: migration 0100 is not in this tree yet")
	}
	return db
}

func exec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.SQL.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed is a small policy written through the legacy tables: the ids of its
// users by name.
type seed struct{ users map[string]int64 }

// seedPolicy writes permissions (one disabled), roles granting them with
// and without scopes, a superuser role, and users: one per role, one with
// two roles, one without, a disabled and a deleted one.
func seedPolicy(t *testing.T, db *store.DB) seed {
	t.Helper()
	exec(t, db, `INSERT INTO permissions (key, module, label, source, status) VALUES
		('cxt:read', 'cxt', '{}', 'core', 'active'),
		('cxt:write', 'cxt', '{}', 'core', 'active'),
		('account:read', 'account', '{}', 'core', 'active'),
		('cxt:old', 'cxt', '{}', 'core', 'disabled')
		ON CONFLICT (key) DO UPDATE SET status = EXCLUDED.status`)
	exec(t, db, `INSERT INTO roles (key, name) VALUES ('cxt_a', '{}'), ('cxt_b', '{}'), ('cxt_old', '{}')`)
	exec(t, db, `INSERT INTO roles (key, name, superuser) VALUES ('cxt_super', '{}', true)`)
	exec(t, db, `INSERT INTO role_permissions (role_id, permission_id)
		SELECT r.id, p.id FROM roles r, permissions p WHERE (r.key, p.key) IN (('cxt_a', 'cxt:read'), ('cxt_b', 'cxt:write'), ('cxt_old', 'cxt:old'))`)
	exec(t, db, `INSERT INTO role_permission_scopes (role_id, permission_id, scope)
		SELECT r.id, p.id, s FROM roles r, permissions p, unnest(ARRAY['group/12', 'own']) s WHERE r.key = 'cxt_b' AND p.key = 'account:read'`)
	s := seed{users: map[string]int64{}}
	for _, u := range []struct {
		name  string
		roles []string
	}{{"a", []string{"cxt_a"}}, {"ab", []string{"cxt_a", "cxt_b"}}, {"none", nil}, {"old", []string{"cxt_old"}},
		{"super", []string{"cxt_super", "cxt_a"}}, {"disabled", []string{"cxt_a"}}, {"deleted", []string{"cxt_b"}}} {
		id := testutil.InsertUser(t, db, "cxt-"+u.name+"@example.com")
		s.users[u.name] = id
		for _, r := range u.roles {
			exec(t, db, `INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE key = $2`, id, r)
		}
	}
	exec(t, db, `UPDATE users SET status = 'disabled' WHERE id = $1`, s.users["disabled"])
	exec(t, db, `UPDATE users SET deleted_at = now() WHERE id = $1`, s.users["deleted"])
	exec(t, db, `UPDATE authz_meta SET version = version + 1 WHERE id = 1`)
	return s
}

func version(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var v int64
	if err := db.SQL.QueryRow(`SELECT version FROM authz_meta WHERE id = 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLoaderPolicy(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	seedPolicy(t, db)
	p, err := casbinx.NewLoader(db).Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := version(t, db); p.Version != want {
		t.Fatalf("version %d, want %d", p.Version, want)
	}
	if !p.Active["cxt:read"] || !p.Active["account:read"] || p.Active["cxt:old"] {
		t.Fatalf("active = %v", p.Active)
	}
	var mine []casbinx.Rule
	for _, r := range p.Rules {
		if r.Ptype != "p" {
			t.Fatalf("a %s row loaded", r.Ptype)
		}
		if strings.HasPrefix(r.V0, "r:cxt_") {
			mine = append(mine, r)
		}
	}
	slices.SortFunc(mine, func(a, b casbinx.Rule) int { return strings.Compare(a.V0+a.V1+a.V2, b.V0+b.V1+b.V2) })
	want := []casbinx.Rule{
		{"p", "r:cxt_a", "cxt:read", "*"},
		{"p", "r:cxt_b", "account:read", "group/12"},
		{"p", "r:cxt_b", "account:read", "own"},
		{"p", "r:cxt_b", "cxt:write", "*"},
		{"p", "r:cxt_old", "cxt:old", "*"},
		{"p", "r:cxt_super", "*", "*"},
	}
	if !slices.Equal(mine, want) {
		t.Fatalf("rules =\n%v\nwant\n%v", mine, want)
	}
}

func TestLoaderUserRoles(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	s := seedPolicy(t, db)
	l := casbinx.NewLoader(db)
	v := version(t, db)
	for name, want := range map[string]casbinx.UserRoles{
		"ab":       {Version: v, Active: true, Roles: []string{"r:cxt_a", "r:cxt_b"}},
		"none":     {Version: v, Active: true},
		"disabled": {Version: v},
		"deleted":  {Version: v},
	} {
		got, err := l.UserRoles(context.Background(), s.users[name])
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != want.Version || got.Active != want.Active || !slices.Equal(got.Roles, want.Roles) {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if got, err := l.UserRoles(context.Background(), 1<<40); err != nil || got.Active || len(got.Roles) != 0 {
		t.Fatalf("unknown user: %+v, %v", got, err)
	}
}

// schemaSQL fingerprints casbin_rule's columns and indexes, and counts the
// tables, so a read through gorm-adapter can be shown to leave the schema
// as the migration made it (AutoMigrate would narrow v1 and v2 to
// varchar(100) and could add tables).
const schemaSQL = `
SELECT string_agg(column_name || ' ' || data_type || COALESCE('(' || character_maximum_length || ')', '') ||
       ' ' || is_nullable || ' ' || COALESCE(column_default, ''), ', ' ORDER BY ordinal_position)
       || ' | ' || (SELECT string_agg(indexdef, '; ' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'casbin_rule')
       || ' | tables ' || (SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema'))
FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'casbin_rule'`

// gorm-adapter is used read-only: the loads leave the schema as it was, and
// run exactly the statements below in the one transaction Policy opens: no
// DDL, no catalog probe of AutoMigrate, no transaction or savepoint of the
// adapter's own.
func TestLoaderAdapterReadOnly(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	seedPolicy(t, db)
	schema := func() string {
		var s string
		if err := db.SQL.QueryRow(schemaSQL).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := schema()
	if !strings.Contains(before, "v1 character varying(") || strings.Contains(before, "v1 character varying(100)") {
		t.Fatalf("casbin_rule.v1 is not the migration's wider column: %s", before)
	}
	traced, log := testutil.Traced(db)
	l := casbinx.NewLoader(traced)
	for range 2 {
		if _, err := l.Policy(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := log.Take()
		want := []*regexp.Regexp{
			regexp.MustCompile(`^begin$`),
			regexp.MustCompile(`^SELECT version FROM authz_meta`),
			regexp.MustCompile(`^SELECT key FROM permissions WHERE status = 'active'$`),
			regexp.MustCompile(`^SELECT .* FROM "casbin_rule" WHERE ptype in \(\$1\) ORDER BY ID$`),
			regexp.MustCompile(`^commit$`),
		}
		if len(got) != len(want) {
			t.Fatalf("statements:\n%s", strings.Join(got, "\n"))
		}
		for i, re := range want {
			if !re.MatchString(strings.TrimSpace(got[i])) {
				t.Fatalf("statement %d = %q, want %s", i, got[i], re)
			}
		}
	}
	if after := schema(); after != before {
		t.Fatalf("schema changed:\nbefore %s\nafter  %s", before, after)
	}
}

// matchesLegacy compares every user's sets as both engines resolve them,
// with the real role cache and the shadow comparison's own normal form
// (authz.Service.CompareEngines: superuser flag, keys, and every scoped key,
// those under an action held for every row included).
func matchesLegacy(t *testing.T, db *store.DB, users map[string]int64) {
	t.Helper()
	az := authz.New(authz.Deps{DB: db, Shadow: authztest.Shadow()})
	for name, uid := range users {
		c, err := az.CompareEngines(context.Background(), uid)
		switch {
		case err != nil:
			t.Fatalf("%s: %v", name, err)
		case c.Skew:
			t.Fatalf("%s: read at v%d and v%d on a quiet database", name, c.Legacy.Version, c.Casbin.Version)
		case c.Mismatch != nil:
			t.Errorf("%s (v%d): legacy only %v, casbin only %v", name, c.Legacy.Version, c.Mismatch.LegacyOnly, c.Mismatch.CasbinOnly)
		}
	}
}

// Through PostgreSQL, the Casbin resolution equals the legacy loader's set
// for every kind of user seeded: superuser flag and keys, the self
// permissions and disabled grants included.
func TestResolverMatchesLegacy(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	matchesLegacy(t, db, seedPolicy(t, db).users)
}

// The same on random roles over the legacy tables (the triggers project
// them): grants of active, disabled and removed permissions, with and
// without scopes (a scope under an action also held for every row
// included), and users of random roles and status.
func TestResolverMatchesLegacyRandom(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	users := seedPolicy(t, db).users
	exec(t, db, `INSERT INTO permissions (key, module, label, source, status) VALUES
		('rnd:a', 'rnd', '{}', 'core', 'active'), ('rnd:b', 'rnd', '{}', 'core', 'active'),
		('rnd:off', 'rnd', '{}', 'core', 'disabled'), ('rnd:gone', 'rnd', '{}', 'core', 'removed')
		ON CONFLICT (key) DO UPDATE SET status = EXCLUDED.status`)
	keys := []string{"rnd:a", "rnd:b", "rnd:off", "rnd:gone", "cxt:read", "account:read"}
	scopes := []string{"own", "group/12", "group/12/account/5", "account/7", "group/*"}
	r := rand.New(rand.NewPCG(7, 11))
	for i := range 8 {
		role := fmt.Sprintf("rnd_%d", i)
		exec(t, db, `INSERT INTO roles (key, name) VALUES ($1, '{}')`, role)
		for _, k := range keys {
			if r.IntN(3) == 0 {
				exec(t, db, `INSERT INTO role_permissions (role_id, permission_id)
					SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = $1 AND p.key = $2`, role, k)
			}
			if k == "account:read" || k == "rnd:a" {
				for _, sc := range scopes {
					if r.IntN(4) == 0 {
						exec(t, db, `INSERT INTO role_permission_scopes (role_id, permission_id, scope)
							SELECT r.id, p.id, $3 FROM roles r, permissions p WHERE r.key = $1 AND p.key = $2`, role, k, sc)
					}
				}
			}
		}
	}
	for i := range 24 {
		name := fmt.Sprintf("rnd-%d", i)
		id := testutil.InsertUser(t, db, name+"@example.com")
		users[name] = id
		for j := range 8 {
			if r.IntN(3) == 0 {
				exec(t, db, `INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE key = $2`, id, fmt.Sprintf("rnd_%d", j))
			}
		}
		switch r.IntN(8) {
		case 0:
			exec(t, db, `UPDATE users SET status = 'disabled' WHERE id = $1`, id)
		case 1:
			exec(t, db, `UPDATE users SET deleted_at = now() WHERE id = $1`, id)
		}
	}
	exec(t, db, `UPDATE authz_meta SET version = version + 1 WHERE id = 1`)
	matchesLegacy(t, db, users)
}
