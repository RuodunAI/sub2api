package dbschema_test

// Transitional, with legacy.go: the conversion of the layouts of cores up
// to 0.1.303. Goes with it.

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// legacyRole sets up plugin key the way cores up to 0.1.303 did, as the
// core's user: a login role marked as this core's, the core a member of
// it, the schema plg_<key> handed to it, the plugin's tables created by it
// with sql (default: items, its sequence and a view) and its migration
// records in plugin_migrations. It returns the role's DSN.
func legacyRole(t *testing.T, db, host *store.DB, hostDSN, key, sql string) string {
	t.Helper()
	ctx := context.Background()
	role := "plg_" + key
	rid := store.QuoteIdent(role)
	var dbname string
	if err := host.SQL.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE ROLE " + rid + " WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 20 PASSWORD 'legacy'",
		"COMMENT ON ROLE " + rid + " IS 'sub2api plugin role; database=" + dbname + "; plugin=" + key + "'",
		"GRANT " + rid + " TO CURRENT_USER",
		"CREATE SCHEMA IF NOT EXISTS " + rid,
		"ALTER SCHEMA " + rid + " OWNER TO " + rid,
		"REVOKE ALL ON SCHEMA " + rid + " FROM PUBLIC",
		"GRANT USAGE, CREATE ON SCHEMA " + rid + " TO " + rid,
		"ALTER ROLE " + rid + " SET search_path TO " + rid,
	} {
		if _, err := host.SQL.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.SQL.ExecContext(c, "DROP SCHEMA IF EXISTS "+rid+" CASCADE")
		_, _ = db.SQL.ExecContext(c, "DROP OWNED BY "+rid)
		_, _ = db.SQL.ExecContext(c, "DROP ROLE IF EXISTS "+rid)
	})
	if sql == "" {
		sql = `CREATE TABLE items (id serial PRIMARY KEY, v text); INSERT INTO items (v) VALUES ('a'), ('b');
			CREATE VIEW item_names AS SELECT v FROM items`
	}
	u, err := url.Parse(hostDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "legacy")
	conn, done, err := testutil.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, sql); err != nil {
		done()
		t.Fatal(err)
	}
	done()
	if _, err := host.SQL.ExecContext(ctx, `INSERT INTO plugin_migrations (plugin_key, migration_id, checksum) VALUES
		($1, '0001_init.sql', 'old'), ($1, 'data:rollout-1:0.1.0->0.2.0', 'data')`, key); err != nil {
		t.Fatal(err)
	}
	// A role with a session is left alone by the conversion.
	waitNoSessions(t, host, role)
	return u.String()
}

// countingHandler counts log records by message, across managers.
type countingHandler struct {
	mu sync.Mutex
	n  map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.n[r.Message]++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[msg]
}

const (
	msgConverted = "converted legacy plugin database role"
	msgBusy      = "legacy plugin database role still has sessions; it is converted once they end"
	msgMoved     = "moved the plugin's tables into the core's schema"
)

func roleExists(t *testing.T, q *store.DB, role string) bool {
	t.Helper()
	var ok bool
	if err := q.SQL.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// assertConverted checks that the legacy role and schema are gone, the
// plugin's tables are in the core's schema under its prefix, owned by
// owner, with their rows, and its SQL migration records are in its own
// table and no longer in plugin_migrations (its data migration record is).
func assertConverted(t *testing.T, db *store.DB, key, owner string) {
	t.Helper()
	ctx := context.Background()
	if roleExists(t, db, "plg_"+key) {
		t.Fatalf("role plg_%s left", key)
	}
	p := manifest.TablePrefix(key)
	var schema bool
	var owners string
	var n, coreRecords, records int
	if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1),
		(SELECT string_agg(DISTINCT relowner::regrole::text, ',') FROM pg_class WHERE relnamespace = 'public'::regnamespace AND starts_with(relname, $2)),
		(SELECT count(*) FROM plugin_migrations WHERE plugin_key = $3 AND migration_id LIKE '%.sql'),
		(SELECT count(*) FROM plugin_migrations WHERE plugin_key = $3)`, "plg_"+key, p, key).Scan(&schema, &owners, &coreRecords, &records); err != nil {
		t.Fatal(err)
	}
	if schema || owners != owner || coreRecords != 0 || records != 1 {
		t.Fatalf("schema left %v, owners %s (want %s), SQL records in plugin_migrations %d, all %d", schema, owners, owner, coreRecords, records)
	}
	var id string
	if err := db.SQL.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM `+p+`item_names), (SELECT migration_id FROM `+p+`schema_migrations WHERE checksum = 'relocated')`).Scan(&n, &id); err != nil || n != 2 || id != "0001_init.sql" {
		t.Fatalf("rows %d, record %q: %v", n, id, err)
	}
}

// A database with the legacy layout (production, or a restore with
// owners): an installed plugin is converted when its database is ensured
// (its tables then belong to its identity), a plugin no longer installed
// by the start-up sweep (its tables stay the core's); a same-named role of
// another database is not touched.
func TestLegacyConverted(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, hostDSN, core := coreUser(t, db, true)
	installed, gone, foreign := randKey(), randKey(), randKey()
	legacyRole(t, db, host, hostDSN, installed, "")
	legacyRole(t, db, host, hostDSN, gone, "")
	frid := store.QuoteIdent("plg_" + foreign)
	if _, err := db.SQL.ExecContext(ctx, "CREATE ROLE "+frid+" LOGIN; COMMENT ON ROLE "+frid+" IS 'sub2api plugin role; database=other; plugin="+foreign+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.SQL.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+frid) })

	logs := &countingHandler{n: map[string]int{}}
	m := newManager(t, db, host, hostDSN, slog.New(logs))
	if err := m.Ensure(ctx, installed); err != nil {
		t.Fatal(err)
	}
	assertConverted(t, db, installed, m.Identity(installed))
	if !roleExists(t, host, "plg_"+gone) {
		t.Fatal("Ensure of one plugin converted another")
	}
	m.ConvertLegacy(ctx)
	assertConverted(t, db, gone, core)
	if !roleExists(t, host, "plg_"+foreign) {
		t.Fatal("a role of another database was dropped")
	}
	if logs.count(msgConverted) != 2 || logs.count(msgMoved) != 2 {
		t.Fatalf("converted %d roles, moved %d schemas, want 2 each", logs.count(msgConverted), logs.count(msgMoved))
	}
	// Idempotent.
	m.ConvertLegacy(ctx)
	if logs.count(msgConverted) != 2 || logs.count(msgMoved) != 2 {
		t.Fatalf("converted again: %v", logs.n)
	}
}

// A restore without owners (everything owned by the core's user, no plugin
// roles) moves the same way.
func TestLegacyWithoutRole(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, hostDSN, core := coreUser(t, db, true)
	key := randKey()
	sid := store.QuoteIdent("plg_" + key)
	if _, err := host.SQL.ExecContext(ctx, "CREATE SCHEMA "+sid+"; CREATE TABLE "+sid+".items (id serial PRIMARY KEY, v text); INSERT INTO "+sid+".items (v) VALUES ('a'), ('b'); CREATE VIEW "+sid+".item_names AS SELECT v FROM "+sid+".items"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.SQL.ExecContext(ctx, `INSERT INTO plugin_migrations (plugin_key, migration_id, checksum) VALUES ($1, '0001_init.sql', 'old'), ($1, 'data:x', 'data')`, key); err != nil {
		t.Fatal(err)
	}
	logs := &countingHandler{n: map[string]int{}}
	m := newManager(t, db, host, hostDSN, slog.New(logs))
	m.ConvertLegacy(ctx)
	assertConverted(t, db, key, core)
	if logs.count(msgConverted) != 0 || logs.count(msgMoved) != 1 {
		t.Fatalf("logged %v", logs.n)
	}
}

// Nodes starting together convert each plugin once, without errors.
func TestLegacyConcurrentStarters(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, hostDSN, _ := coreUser(t, db, true)
	keys := []string{randKey(), randKey(), randKey()}
	for _, k := range keys {
		legacyRole(t, db, host, hostDSN, k, "")
	}
	logs := &countingHandler{n: map[string]int{}}
	var failed atomic.Int32
	var wg sync.WaitGroup
	var managers []*dbschema.Manager
	for range 4 {
		managers = append(managers, newManager(t, db, host, hostDSN, slog.New(logs)))
	}
	for _, m := range managers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ConvertLegacy(ctx)
			for _, k := range keys {
				if err := m.Ensure(ctx, k); err != nil {
					t.Error(err)
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.FailNow()
	}
	if logs.count(msgConverted) != len(keys) || logs.count(msgMoved) != len(keys) {
		t.Fatalf("logs %v, want %d conversions", logs.n, len(keys))
	}
	for _, k := range keys {
		assertConverted(t, db, k, managers[0].Identity(k))
	}
}

// A role with an open session (an old plugin process) is left, logged
// once, and so are its tables: Ensure fails until the session ends, and
// the old process keeps its tables meanwhile.
func TestLegacyRoleBusy(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, hostDSN, _ := coreUser(t, db, true)
	key := randKey()
	roleDSN := legacyRole(t, db, host, hostDSN, key, "")
	old, done, err := testutil.Connect(ctx, roleDSN)
	if err != nil {
		t.Fatal(err)
	}
	logs := &countingHandler{n: map[string]int{}}
	m := newManager(t, db, host, hostDSN, slog.New(logs))
	for range 2 {
		if err := m.Ensure(ctx, key); err == nil || !strings.Contains(err.Error(), "still has sessions") {
			t.Fatalf("Ensure with a busy legacy role: %v", err)
		}
	}
	m.ConvertLegacy(ctx)
	if !roleExists(t, host, "plg_"+key) || logs.count(msgBusy) != 1 {
		t.Fatalf("busy role: exists=%v logged=%d", roleExists(t, host, "plg_"+key), logs.count(msgBusy))
	}
	var n int
	if err := old.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("old process lost its table: %d %v", n, err)
	}
	done()
	waitNoSessions(t, host, "plg_"+key)
	if err := m.Ensure(ctx, key); err != nil {
		t.Fatal(err)
	}
	assertConverted(t, db, key, m.Identity(key))

	// Purge of a legacy plugin, without an Ensure first.
	other := randKey()
	legacyRole(t, db, host, hostDSN, other, "")
	if err := m.Drop(ctx, other, true); err != nil {
		t.Fatal(err)
	}
	var left bool
	if err := host.SQL.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)
		OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)
		OR EXISTS (SELECT 1 FROM pg_class WHERE starts_with(relname, $2))
		OR EXISTS (SELECT 1 FROM plugin_migrations WHERE plugin_key = $3)`, "plg_"+other, manifest.TablePrefix(other), other).Scan(&left); err != nil || left {
		t.Fatalf("left after purge: %v %v", left, err)
	}
}

// A schema with an object that cannot move (a function) stays as it is,
// the plugin's Ensure fails, and the other plugins move.
func TestLegacyRelocationAborts(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	host, hostDSN, core := coreUser(t, db, true)
	bad, good := randKey(), randKey()
	legacyRole(t, db, host, hostDSN, bad, `CREATE TABLE items (id serial PRIMARY KEY, v text); INSERT INTO items (v) VALUES ('a'), ('b');
		CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
	legacyRole(t, db, host, hostDSN, good, "")
	m := newManager(t, db, host, hostDSN, nil)
	m.ConvertLegacy(ctx)
	assertConverted(t, db, good, core)
	var n int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM "plg_`+bad+`".items`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the unmovable plugin's table: %d %v", n, err)
	}
	if err := m.Ensure(ctx, bad); err == nil || !strings.Contains(err.Error(), "cannot move") {
		t.Fatalf("Ensure: %v", err)
	}
}

// The relocation of a legacy plugin ends in the structure a fresh install
// of the rewritten scripts has - relations, columns, defaults, constraint
// and index names and definitions, views, owners, grants and migration
// records - with its rows, and its scripts do not run again.
func TestRelocationMatchesFreshInstall(t *testing.T) {
	ctx := context.Background()
	key := randKey()
	p := manifest.TablePrefix(key)
	// The scripts as cores up to 0.1.303 ran them ({p} empty) and as
	// rewritten for the core's schema ({p} the prefix): the same names
	// prefixed, a GORM foreign key name fk_<table>_<field> on the new
	// table name.
	const script = `CREATE TABLE {p}items (id serial PRIMARY KEY, v text NOT NULL CHECK (v <> ''), parent int REFERENCES {p}items (id));
		CREATE INDEX {p}items_v_idx ON {p}items (v);
		CREATE TABLE {p}orders (id bigserial PRIMARY KEY, item_id int, n int DEFAULT 1,
			CONSTRAINT fk_{p}orders_item FOREIGN KEY (item_id) REFERENCES {p}items (id), CONSTRAINT {p}orders_n_chk CHECK (n > 0));
		CREATE UNIQUE INDEX {p}orders_item_uq ON {p}orders (item_id);
		CREATE SEQUENCE {p}ticket_seq;
		CREATE VIEW {p}item_names AS SELECT v FROM {p}items;
		INSERT INTO {p}items (v) VALUES ('seed');`
	old := strings.ReplaceAll(script, "{p}", "")
	fresh := strings.ReplaceAll(script, "{p}", p)
	newScripts := scripts("0001_init.sql", fresh)

	// The legacy database, relocated at start and migrated with the
	// rewritten scripts.
	dbOld := testutil.DB(t)
	hostOld, dsnOld, _ := coreUser(t, dbOld, true)
	legacyRole(t, dbOld, hostOld, dsnOld, key, old)
	install(t, dbOld, key, []manifest.DatabaseAccess{{Table: "users", Columns: []string{"id"}, Privileges: []string{"select"}}}, true)
	mOld := newManager(t, dbOld, hostOld, dsnOld, nil)
	mOld.ConvertLegacy(ctx)
	if applied, err := mOld.Migrate(ctx, key, newScripts); err != nil || len(applied) != 0 {
		t.Fatalf("migrate after the relocation: %v %v", applied, err)
	}

	// A fresh database.
	dbNew := testutil.DB(t)
	hostNew, dsnNew, _ := coreUser(t, dbNew, true)
	install(t, dbNew, key, []manifest.DatabaseAccess{{Table: "users", Columns: []string{"id"}, Privileges: []string{"select"}}}, true)
	mNew := newManager(t, dbNew, hostNew, dsnNew, nil)
	if applied, err := mNew.Migrate(ctx, key, newScripts); err != nil || len(applied) != 1 {
		t.Fatalf("fresh install: %v %v", applied, err)
	}

	got, want := structure(t, dbOld, mOld, key), structure(t, dbNew, mNew, key)
	if !slices.Equal(got, want) {
		var diff []string
		for _, l := range got {
			if !slices.Contains(want, l) {
				diff = append(diff, "relocated only: "+l)
			}
		}
		for _, l := range want {
			if !slices.Contains(got, l) {
				diff = append(diff, "fresh only:     "+l)
			}
		}
		t.Fatalf("structures differ:\n%s", strings.Join(diff, "\n"))
	}
	var n int
	if err := dbOld.SQL.QueryRowContext(ctx, `SELECT count(*) FROM `+p+`items`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after the relocation: %d %v", n, err)
	}
}

// structure lists the plugin's objects in db as comparable lines, the
// identity's name replaced by "identity".
func structure(t *testing.T, db *store.DB, m *dbschema.Manager, key string) []string {
	t.Helper()
	rows, err := db.SQL.QueryContext(context.Background(), `
		SELECT 'rel ' || c.relname || ' ' || c.relkind::text || ' ' || replace(c.relowner::regrole::text, $2, 'identity')
			|| ' ' || COALESCE(replace(c.relacl::text, $2, 'identity'), '')
		FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND starts_with(c.relname, $1)
		UNION ALL
		SELECT 'col ' || c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || ' ' || a.attnotnull
			|| ' ' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relnamespace = 'public'::regnamespace AND starts_with(c.relname, $1) AND a.attnum > 0 AND NOT a.attisdropped
		UNION ALL
		SELECT 'con ' || c.relname || ' ' || k.conname || ' ' || pg_get_constraintdef(k.oid)
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid WHERE c.relnamespace = 'public'::regnamespace AND starts_with(c.relname, $1)
		UNION ALL
		SELECT 'idx ' || pg_get_indexdef(i.indexrelid)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid WHERE c.relnamespace = 'public'::regnamespace AND starts_with(c.relname, $1)
		UNION ALL
		SELECT 'view ' || c.relname || ' ' || pg_get_viewdef(c.oid)
		FROM pg_class c WHERE c.relkind = 'v' AND c.relnamespace = 'public'::regnamespace AND starts_with(c.relname, $1)
		UNION ALL
		SELECT 'grant ' || c.relname || ' ' || a.privilege_type
		FROM pg_class c, aclexplode(c.relacl) a WHERE a.grantee = $2::regrole AND NOT starts_with(c.relname, $1)
		UNION ALL
		SELECT 'rec ' || migration_id || ' ' || checksum FROM `+store.QuoteIdent(manifest.TablePrefix(key)+"schema_migrations")+`
		ORDER BY 1`, manifest.TablePrefix(key), m.Identity(key))
	if err != nil {
		t.Fatal(err)
	}
	lines, err := testutil.Column[string](rows)
	if err != nil || len(lines) < 20 {
		t.Fatalf("structure: %d lines, %v", len(lines), err)
	}
	return lines
}

// waitNoSessions waits until the backend of a closed connection is gone
// from pg_stat_activity (closing a client does not wait for it).
func waitNoSessions(t *testing.T, q *store.DB, role string) {
	t.Helper()
	ctx := context.Background()
	for {
		var n int
		if err := q.SQL.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename = $1`, role).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if _, err := q.SQL.ExecContext(ctx, `SELECT pg_sleep(0.05)`); err != nil {
			t.Fatal(err)
		}
	}
}

// The relocation names constraints as the rewritten scripts do: GORM's
// foreign keys fk_<table>_<field> on the new table name, the rest with the
// prefix in front.
func TestLegacyConstraintName(t *testing.T) {
	p := manifest.TablePrefix("payment")
	for from, want := range map[string]string{
		"fk_payment_orders_provider_instance": "fk_plg_payment__payment_orders_provider_instance",
		"fk_payment_audit_logs_order":         "fk_plg_payment__payment_audit_logs_order",
		"fk_promo_code_usage_promo_code":      "fk_plg_payment__promo_code_usage_promo_code",
		"orders_amount_check":                 "plg_payment__orders_amount_check",
	} {
		if got := dbschema.LegacyConstraintName(p, from); got != want {
			t.Errorf("%s: %s, want %s", from, got, want)
		}
	}
}
