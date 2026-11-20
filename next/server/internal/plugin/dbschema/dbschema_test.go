package dbschema_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// A plugin's migrations run as its identity, in the core's schema, under
// its prefix; afterwards the identity reads and writes its own tables and
// nothing else, and creates nothing.
func TestIsolation(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, dsn, core := coreUser(t, db, true)
	m := newManager(t, db, host, dsn, nil)
	key, other := randKey(), randKey()
	install(t, db, key, nil, false)
	install(t, db, other, nil, false)
	p := manifest.TablePrefix(key)
	op := manifest.TablePrefix(other)

	applied, err := m.Migrate(ctx, key, scripts(
		"0001_init.sql", `CREATE TABLE `+p+`items (id serial PRIMARY KEY, v text); INSERT INTO `+p+`items (v) VALUES ('a');
			SELECT current_user;`,
	))
	if err != nil || len(applied) != 1 {
		t.Fatalf("migrate: %v %v", applied, err)
	}
	if _, err := m.Migrate(ctx, other, scripts("0001_init.sql", `CREATE TABLE `+op+`secrets (v text); INSERT INTO `+op+`secrets VALUES ('s')`)); err != nil {
		t.Fatal(err)
	}
	if again, err := m.Migrate(ctx, key, scripts("0001_init.sql", `CREATE TABLE `+p+`items (id serial PRIMARY KEY, v text); INSERT INTO `+p+`items (v) VALUES ('a');
			SELECT current_user;`)); err != nil || len(again) != 0 {
		t.Fatalf("second migrate: %v %v", again, err)
	}
	if pending, err := m.Pending(ctx, key, []string{"0001_init.sql"}); err != nil || pending {
		t.Fatalf("pending = %v %v", pending, err)
	}
	if pending, _ := m.Pending(ctx, key, []string{"0001_init.sql", "0002.sql"}); !pending {
		t.Fatal("0002 should be pending")
	}

	// The tables, their sequence and the records belong to the identity;
	// it has no CREATE after the migration; the records are its own table,
	// not plugin_migrations.
	role := m.Identity(key)
	var owners []string
	rows, err := db.SQL.QueryContext(ctx, `SELECT DISTINCT relowner::regrole::text FROM pg_class WHERE relnamespace = 'public'::regnamespace AND starts_with(relname, $1)`, p)
	if err != nil {
		t.Fatal(err)
	}
	if owners, err = testutil.Column[string](rows); err != nil || len(owners) != 1 || owners[0] != role {
		t.Fatalf("owners of the plugin's objects: %v %v (identity %s)", owners, err, role)
	}
	var canCreate, records bool
	if err := db.SQL.QueryRowContext(ctx, `SELECT has_schema_privilege($1, 'public', 'CREATE'),
		EXISTS (SELECT 1 FROM plugin_migrations WHERE plugin_key = $2)`, role, key).Scan(&canCreate, &records); err != nil || canCreate || records {
		t.Fatalf("CREATE after the migration %v, records in plugin_migrations %v: %v", canCreate, records, err)
	}

	pl := asPlugin(t, m, dsn, key)
	var who, sp string
	var n int
	if err := pl.SQL.QueryRowContext(ctx, `INSERT INTO `+p+`items (v) VALUES ('b') RETURNING current_user, current_setting('search_path'), (SELECT count(*) FROM `+p+`items)`).Scan(&who, &sp, &n); err != nil || who != role {
		t.Fatalf("own table as %s: %v", who, err)
	}
	for _, q := range []string{
		`SELECT * FROM users`,
		`UPDATE users SET status = status`,
		`INSERT INTO plugins (key, name, status) VALUES ('x', '{}', 'installed')`,
		`SELECT * FROM ` + op + `secrets`,
		`DELETE FROM ` + op + `secrets`,
		`CREATE TABLE ` + p + `later (id int)`,
		`CREATE TEMP TABLE scratch (id int) ON COMMIT DROP; CREATE TABLE public.` + p + `x (id int)`,
		`ALTER TABLE users ADD COLUMN x int`,
		`SET ROLE "` + core + `"`,
	} {
		if err := queryErr(pl, q); err == nil {
			t.Fatalf("the plugin's identity ran %s", q)
		}
	}
}

// A script that misses the prefix on a core table is refused by the
// database; one that creates an object without the prefix - also in a
// subtransaction or through a function - is rolled back whole. CREATE is
// taken back either way.
func TestMigratePrefixCheck(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, dsn, _ := coreUser(t, db, true)
	m := newManager(t, db, host, dsn, nil)
	key := randKey()
	install(t, db, key, nil, false)
	p := manifest.TablePrefix(key)
	for _, c := range []struct{ name, sql, want string }{
		{"core table", `CREATE TABLE ` + p + `a (id int); ALTER TABLE users ADD COLUMN x int`, "must be owner of table users"},
		{"no prefix", `CREATE TABLE ` + p + `a (id int); CREATE TABLE items (id int)`, "without the plugin's prefix"},
		{"index", `CREATE TABLE ` + p + `a (id int); CREATE INDEX a_id ON ` + p + `a (id)`, "a_id"},
		{"subtransaction", `CREATE TABLE ` + p + `a (id int); DO $$ BEGIN CREATE TABLE sneaky (id int); EXCEPTION WHEN others THEN NULL; END $$`, "sneaky"},
		{"function", `CREATE TABLE ` + p + `a (id int); CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'`, "without the plugin's prefix"},
		{"type", `CREATE TABLE ` + p + `a (id int); CREATE TYPE mood AS ENUM ('a')`, "mood"},
		{"rename", `CREATE TABLE ` + p + `a (id int); ALTER TABLE ` + p + `a RENAME TO a`, "without the plugin's prefix"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := m.Migrate(ctx, key, scripts("0001.sql", c.sql))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("migrate: %v, want %q", err, c.want)
			}
			var left int
			var canCreate bool
			if err := db.SQL.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ($1, 'items', 'sneaky', 'a', 'a_id'))
				+ (SELECT count(*) FROM pg_proc WHERE proname = 'f') + (SELECT count(*) FROM pg_type WHERE typname = 'mood'),
				has_schema_privilege($2, 'public', 'CREATE')`, p+"a", m.Identity(key)).Scan(&left, &canCreate); err != nil {
				t.Fatal(err)
			}
			if left != 0 || canCreate {
				t.Fatalf("left %d objects, CREATE %v", left, canCreate)
			}
		})
	}
	// Prefixed objects of every kind pass; the records table carries the
	// prefix too.
	if _, err := m.Migrate(ctx, key, scripts("0001.sql", `CREATE TABLE `+p+`a (id serial PRIMARY KEY, v text CHECK (v <> ''));
		CREATE INDEX `+p+`a_v ON `+p+`a (v); CREATE VIEW `+p+`names AS SELECT v FROM `+p+`a;
		CREATE TYPE `+p+`mood AS ENUM ('a'); CREATE FUNCTION `+p+`one() RETURNS int LANGUAGE sql AS 'SELECT 1'`)); err != nil {
		t.Fatal(err)
	}
}

// The tables and columns a plugin was granted (db.core_read) are readable
// as asked, insert included (with the sequence nextval needs); the rest is
// not. A revocation of the grant takes effect in its transaction.
func TestAccess(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, dsn, _ := coreUser(t, db, true)
	m := newManager(t, db, host, dsn, nil)
	key := randKey()
	install(t, db, key, []manifest.DatabaseAccess{
		{Table: "users", Columns: []string{"id", "email"}, Privileges: []string{"select"}},
		{Table: "audit_logs", Privileges: []string{"select", "insert"}},
	}, true)
	if errs, err := m.CheckAccess(ctx, []manifest.DatabaseAccess{
		{Table: "users", Columns: []string{"id", "nope"}}, {Table: "nope"}, {Table: "groups"},
	}); err != nil || len(errs) != 2 || errs[0].Field != "database.access[0].columns[1]" || errs[1].Field != "database.access[1].table" {
		t.Fatalf("CheckAccess: %+v %v", errs, err)
	}
	if err := m.Ensure(ctx, key); err != nil {
		t.Fatal(err)
	}
	pl := asPlugin(t, m, dsn, key)
	for _, q := range []string{
		`SELECT id, email FROM users`,
		`SELECT * FROM audit_logs`,
		`INSERT INTO audit_logs (action, target_type, target_id) VALUES ('x', 'y', 'z')`,
	} {
		if err := queryErr(pl, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`SELECT password_hash FROM users`,
		`SELECT * FROM users`,
		`UPDATE users SET email = email`,
		`DELETE FROM audit_logs`,
		`SELECT * FROM groups`,
	} {
		if err := queryErr(pl, q); err == nil {
			t.Fatalf("the plugin ran %s", q)
		}
	}
	// Idempotent: nothing to change, nothing written.
	if _, err := m.Migrate(ctx, key, nil); err != nil {
		t.Fatal(err)
	}
	// The revocation of the grant (install.RevokeGrant) syncs in its
	// transaction.
	if err := host.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE plugin_permission_grants SET status = 'revoked' WHERE plugin_key = ?`, key).Error; err != nil {
			return err
		}
		return m.SyncAccessTx(ctx, tx, key)
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT id FROM users`, `SELECT * FROM audit_logs`} {
		if err := queryErr(pl, q); err == nil {
			t.Fatalf("after the revocation the plugin ran %s", q)
		}
	}
}

// Ensure brings a damaged database side back: a dropped identity is created
// again and gets its tables back, a table handed to the core (a restore
// without owners) returns to the identity.
func TestEnsureRecovers(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, dsn, core := coreUser(t, db, true)
	m := newManager(t, db, host, dsn, nil)
	key := randKey()
	install(t, db, key, nil, false)
	p := manifest.TablePrefix(key)
	if _, err := m.Migrate(ctx, key, scripts("0001.sql", `CREATE TABLE `+p+`items (id serial PRIMARY KEY, v text); INSERT INTO `+p+`items (v) VALUES ('a')`)); err != nil {
		t.Fatal(err)
	}
	role := store.QuoteIdent(m.Identity(key))
	owner := func() string {
		var o string
		if err := db.SQL.QueryRowContext(ctx, `SELECT relowner::regrole::text FROM pg_class WHERE relname = $1`, p+"items").Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	check := func() {
		t.Helper()
		if _, err := m.Migrate(ctx, key, nil); err != nil {
			t.Fatal(err)
		}
		if o := owner(); o != m.Identity(key) {
			t.Fatalf("owner %s", o)
		}
		var n int
		if err := asPlugin(t, m, dsn, key).SQL.QueryRowContext(ctx, `SELECT count(*) FROM `+p+`items`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("read as the identity: %d %v", n, err)
		}
	}
	// The table handed to the core.
	if _, err := db.SQL.ExecContext(ctx, `ALTER TABLE `+p+`items OWNER TO "`+core+`"`); err != nil {
		t.Fatal(err)
	}
	check()
	// The identity dropped, its tables kept.
	if _, err := db.SQL.ExecContext(ctx, `REASSIGN OWNED BY `+role+` TO "`+core+`"; DROP OWNED BY `+role+`; DROP ROLE `+role); err != nil {
		t.Fatal(err)
	}
	check()
}

// Without CREATEROLE the core cannot give a plugin its identity: the plugin
// does not get a database.
func TestNoCreateRole(t *testing.T) {
	db := testutil.DB(t)
	host, dsn, _ := coreUser(t, db, false)
	m := newManager(t, db, host, dsn, nil)
	key := randKey()
	if _, _, err := m.DSN(context.Background(), key); !errors.Is(err, dbschema.ErrIsolationUnavailable) {
		t.Fatalf("DSN: %v", err)
	}
	if _, err := m.Migrate(context.Background(), key, scripts("0001.sql", "SELECT 1")); !errors.Is(err, dbschema.ErrIsolationUnavailable) {
		t.Fatalf("Migrate: %v", err)
	}
}

// Uninstall keeping the data leaves the tables to the core and drops the
// identity; a reinstall gets them back with their records. Uninstall with
// purge drops everything of the plugin's, also after a keeping one.
func TestDrop(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, dsn, core := coreUser(t, db, true)
	m := newManager(t, db, host, dsn, nil)
	key := randKey()
	install(t, db, key, []manifest.DatabaseAccess{{Table: "users", Columns: []string{"id"}, Privileges: []string{"select"}}}, true)
	p := manifest.TablePrefix(key)
	fsys := scripts("0001.sql", `CREATE TABLE `+p+`items (id serial PRIMARY KEY, v text); INSERT INTO `+p+`items (v) VALUES ('a')`)
	if _, err := m.Migrate(ctx, key, fsys); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO plugin_migrations (plugin_key, migration_id, checksum) VALUES ($1, 'data:x', 'data')`, key); err != nil {
		t.Fatal(err)
	}
	state := func() (role bool, objects int, owner string, records int) {
		t.Helper()
		if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
			(SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND starts_with(relname, $2)),
			COALESCE((SELECT relowner::regrole::text FROM pg_class WHERE relname = $2 || 'items'), ''),
			(SELECT count(*) FROM plugin_migrations WHERE plugin_key = $3)`, m.Identity(key), p, key).Scan(&role, &objects, &owner, &records); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := m.Drop(ctx, key, false); err != nil {
		t.Fatal(err)
	}
	if role, objects, owner, records := state(); role || objects == 0 || owner != core || records != 1 {
		t.Fatalf("kept: role %v, %d objects, owner %s, %d core records", role, objects, owner, records)
	}
	// Reinstall: the records say the script ran, the table is the identity's.
	applied, err := m.Migrate(ctx, key, fsys)
	if err != nil || len(applied) != 0 {
		t.Fatalf("reinstall: %v %v", applied, err)
	}
	if role, _, owner, _ := state(); !role || owner != m.Identity(key) {
		t.Fatalf("reinstalled: role %v owner %s", role, owner)
	}
	var n int
	if err := asPlugin(t, m, dsn, key).SQL.QueryRowContext(ctx, `SELECT count(*) FROM `+p+`items, users`).Scan(&n); err != nil {
		t.Fatalf("read after the reinstall: %v", err)
	}
	// Purge after a keeping uninstall: the tables are the core's then.
	if err := m.Drop(ctx, key, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Drop(ctx, key, true); err != nil {
		t.Fatal(err)
	}
	if role, objects, _, records := state(); role || objects != 0 || records != 0 {
		t.Fatalf("purged: role %v, %d objects, %d core records", role, objects, records)
	}
	// Purge of an installed plugin: the identity's grants on core tables go.
	if _, err := m.Migrate(ctx, key, fsys); err != nil {
		t.Fatal(err)
	}
	if err := m.Drop(ctx, key, true); err != nil {
		t.Fatal(err)
	}
	if role, objects, _, _ := state(); role || objects != 0 {
		t.Fatalf("purged: role %v, %d objects", role, objects)
	}
}

// A database created by PostgreSQL 14 or older lets every role create
// tables in public: the manager takes that back at start.
func TestPublicCreateRevoked(t *testing.T) {
	db := testutil.DB(t)
	host, dsn, _ := coreUser(t, db, true)
	if _, err := db.SQL.ExecContext(context.Background(), `GRANT CREATE ON SCHEMA public TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	newManager(t, db, host, dsn, nil)
	var open bool
	if err := db.SQL.QueryRowContext(context.Background(), `SELECT has_schema_privilege('public', 'public', 'CREATE')`).Scan(&open); err != nil || open {
		t.Fatalf("PUBLIC may still create: %v %v", open, err)
	}
}

// Every SQL migration shipped with an official plugin (and its test
// overlays) is idempotent: applied twice in order, the second pass changes
// nothing and fails nowhere. The core records each file once, but a lost
// record or a corrected script runs it again.
func TestOfficialPluginMigrationsAreIdempotent(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	root := filepath.Join("..", "..", "..", "..", "plugins")
	dirs, err := filepath.Glob(filepath.Join(root, "*", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	overlays, _ := filepath.Glob(filepath.Join(root, "*", "testdata", "*", "migrations"))
	if len(dirs) < 4 {
		t.Fatalf("found only %v under %s", dirs, root)
	}
	for _, dir := range dirs {
		plugin := filepath.Base(filepath.Dir(dir))
		sets := [][]string{}
		base, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
		sort.Strings(base)
		sets = append(sets, base)
		for _, o := range overlays {
			if strings.Contains(o, string(filepath.Separator)+plugin+string(filepath.Separator)) {
				extra, _ := filepath.Glob(filepath.Join(o, "*.sql"))
				sort.Strings(extra)
				sets = append(sets, append(append([]string(nil), base...), extra...))
			}
		}
		for _, files := range sets {
			t.Run(plugin+"/"+strings.Join(names(files), ","), func(t *testing.T) {
				tx, err := testutil.Begin(ctx, db)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				schema := store.QuoteIdent("idem_" + randKey())
				if _, err = store.SQLTx(tx).ExecContext(ctx, "CREATE SCHEMA "+schema+"; SET LOCAL search_path TO "+schema); err != nil {
					t.Fatal(err)
				}
				for pass := 1; pass <= 2; pass++ {
					for _, f := range files {
						body, err := os.ReadFile(f)
						if err != nil {
							t.Fatal(err)
						}
						if _, err = store.SQLTx(tx).ExecContext(ctx, string(body)); err != nil {
							t.Fatalf("pass %d of %s: %v", pass, filepath.Base(f), err)
						}
					}
				}
			})
		}
	}
}

func names(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = filepath.Base(f)
	}
	return out
}
