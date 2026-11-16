package casbinx_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// policyDB is a migrated database: casbin_rule as migration 0100 makes it.
func policyDB(t *testing.T) *store.DB {
	t.Helper()
	return testutil.DB(t)
}

func exec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.SQL.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed is a small policy written as casbin_rule rows: the ids of its
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
	exec(t, db, `INSERT INTO roles (key, name, superuser) VALUES ($1, '{}', true)`, authz.RoleSuperAdmin)
	testutil.Grant(t, db, "cxt_a", "cxt:read")
	testutil.Grant(t, db, "cxt_b", "cxt:write")
	testutil.Grant(t, db, "cxt_old", "cxt:old")
	testutil.Grant(t, db, "cxt_b", "account:read", "group/12", "own")
	testutil.Grant(t, db, authz.RoleSuperAdmin, casbinx.SuperKey)
	s := seed{users: map[string]int64{}}
	for _, u := range []struct {
		name  string
		roles []string
	}{{"a", []string{"cxt_a"}}, {"ab", []string{"cxt_a", "cxt_b"}}, {"none", nil}, {"old", []string{"cxt_old"}},
		{"super", []string{authz.RoleSuperAdmin, "cxt_a"}}, {"disabled", []string{"cxt_a"}}, {"deleted", []string{"cxt_b"}}} {
		id := testutil.InsertUser(t, db, "cxt-"+u.name+"@example.com")
		s.users[u.name] = id
		testutil.AssignRoles(t, db, id, u.roles...)
	}
	exec(t, db, `UPDATE users SET status = 'disabled' WHERE id = $1`, s.users["disabled"])
	exec(t, db, `UPDATE users SET deleted_at = now() WHERE id = $1`, s.users["deleted"])
	testutil.BumpAuthz(t, db)
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
	s := seedPolicy(t, db)
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
	links := map[string][]string{}
	for _, r := range p.Rules {
		switch {
		case r.Ptype == "p" && strings.HasPrefix(r.V0, "r:cxt_"):
			mine = append(mine, r)
		case r.Ptype == "g":
			links[r.V0] = append(links[r.V0], r.V1)
		}
	}
	// Every g row is read; the enforcer links the active users' only.
	if got := links[casbinrule.UserSub(s.users["deleted"])]; !slices.Equal(got, []string{"r:cxt_b"}) {
		t.Fatalf("g rows of the deleted user: %v", got)
	}
	for name, want := range map[string]bool{"a": true, "none": true, "disabled": false, "deleted": false} {
		if got := slices.Contains(p.Users, s.users[name]); got != want {
			t.Errorf("%s active: %v", name, got)
		}
	}
	slices.SortFunc(mine, func(a, b casbinx.Rule) int { return strings.Compare(a.V0+a.V1+a.V2, b.V0+b.V1+b.V2) })
	want := []casbinx.Rule{
		{"p", "r:cxt_a", "cxt:read", "*"},
		{"p", "r:cxt_b", "account:read", "group/12"},
		{"p", "r:cxt_b", "account:read", "own"},
		{"p", "r:cxt_b", "cxt:write", "*"},
		{"p", "r:cxt_old", "cxt:old", "*"},
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
			regexp.MustCompile(`^SELECT version, policy_version FROM authz_meta`),
			regexp.MustCompile(`^SELECT "key" FROM "permissions" WHERE status = 'active'$`),
			regexp.MustCompile(`^SELECT "id" FROM "users" WHERE deleted_at IS NULL AND status = 'active'$`),
			regexp.MustCompile(`^SELECT .* FROM "casbin_rule" WHERE ptype in \(\$1,\$2\) ORDER BY ID$`),
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

// Through PostgreSQL, the decisions and lists of each kind of user seeded:
// grants with and without scopes, the self permissions of an active user,
// none of a disabled permission, every key for the superuser, nothing for
// a disabled or deleted user.
func TestEnforcerThroughPostgres(t *testing.T) {
	t.Parallel()
	db := policyDB(t)
	s := seedPolicy(t, db)
	r := casbinx.New(casbinx.NewLoader(db), []string{"self:read"}, casbinx.Options{})
	v := version(t, db)
	ctx := context.Background()
	for name, want := range map[string][]string{
		"a":        {"cxt:read", "self:read"},
		"ab":       {"account:read:group/12", "account:read:own", "cxt:read", "cxt:write", "self:read"},
		"none":     {"self:read"},
		"old":      {"self:read"},
		"disabled": {},
		"deleted":  {},
	} {
		got, err := r.Permissions(ctx, s.users[name], v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Superuser || !slices.Equal(got.KeyList(), want) {
			t.Errorf("%s: superuser %v, keys %v, want %v", name, got.Superuser, got.KeyList(), want)
		}
		for _, k := range append(want, "cxt:old", "account:read") {
			ok, err := r.Enforce(ctx, s.users[name], v, k)
			if err != nil || ok != slices.Contains(want, k) {
				t.Errorf("%s: Enforce(%s) = %v, %v", name, k, ok, err)
			}
		}
	}
	got, err := r.Permissions(ctx, s.users["super"], v)
	if err != nil || !got.Superuser || !got.Has("cxt:a-key-to-come") {
		t.Fatalf("super: %+v, %v", got, err)
	}
}
