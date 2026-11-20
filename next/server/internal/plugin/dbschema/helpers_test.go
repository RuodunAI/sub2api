package dbschema_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func randKey() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "ds" + hex.EncodeToString(b)
}

// coreUser connects to the test database as a fresh login role that is not
// a superuser, like the core's user on a managed PostgreSQL: it owns the
// database (and so the public schema, through pg_database_owner) and holds
// every privilege on the core's tables with the right to grant them, as
// the owner of the tables would. createRole gives it CREATEROLE.
func coreUser(t *testing.T, db *store.DB, createRole bool) (host *store.DB, dsn, name string) {
	t.Helper()
	ctx := context.Background()
	name = "core_" + randKey()
	pw := "pw-" + randKey()
	rid := store.QuoteIdent(name)
	var dbname string
	if err := db.SQL.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	attr := "NOCREATEROLE"
	if createRole {
		attr = "CREATEROLE"
	}
	for _, s := range []string{
		"CREATE ROLE " + rid + " LOGIN " + attr + " NOSUPERUSER PASSWORD '" + pw + "'",
		"ALTER DATABASE " + store.QuoteIdent(dbname) + " OWNER TO " + rid,
		"GRANT ALL ON ALL TABLES IN SCHEMA public TO " + rid + " WITH GRANT OPTION",
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO " + rid + " WITH GRANT OPTION",
	} {
		if _, err := db.SQL.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(testutil.DSN(db))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(name, pw)
	host, err = store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	var super bool
	if err := host.SQL.QueryRowContext(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil || super {
		t.Fatalf("core user is a superuser: %v %v", super, err)
	}
	t.Cleanup(func() {
		host.Close()
		c := context.Background()
		_, _ = db.SQL.ExecContext(c, "ALTER DATABASE "+store.QuoteIdent(dbname)+" OWNER TO CURRENT_USER")
		_, _ = db.SQL.ExecContext(c, "DROP OWNED BY "+rid)
		_, _ = db.SQL.ExecContext(c, "DROP ROLE IF EXISTS "+rid)
	})
	return host, u.String(), name
}

// newManager builds the manager on host (the core's pool) with a fresh
// master key and drops the identities it creates in this database at the
// end, through db (a superuser's pool).
func newManager(t *testing.T, db, host *store.DB, dsn string, log *slog.Logger) *dbschema.Manager {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	m, err := dbschema.New(context.Background(), host, dsn, mk, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropIdentities(db) })
	return m
}

// dropIdentities drops the plugin identities of db's database.
func dropIdentities(db *store.DB) {
	ctx := context.Background()
	rows, err := db.SQL.QueryContext(ctx, `SELECT rolname FROM pg_roles
		WHERE rolname LIKE 'plg' || (SELECT oid FROM pg_database WHERE datname = current_database()) || '\_%'`)
	if err != nil {
		return
	}
	roles, _ := testutil.Column[string](rows)
	for _, r := range roles {
		_, _ = db.SQL.ExecContext(ctx, "DROP OWNED BY "+store.QuoteIdent(r)+"; DROP ROLE "+store.QuoteIdent(r))
	}
}

// asPlugin opens a pool logged in as the plugin's identity, as the core's
// proxy does for it.
func asPlugin(t *testing.T, m *dbschema.Manager, dsn, key string) *store.DB {
	t.Helper()
	p, err := store.OpenAs(context.Background(), dsn, m.Identity(key), m.Password(key), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// install records plugin key at version 1.0.0, active, asking for access,
// with db.core_read granted (scope as the consent records it) when grant.
func install(t *testing.T, db *store.DB, key string, access []manifest.DatabaseAccess, grant bool) {
	t.Helper()
	g := store.G(context.Background(), db.Gorm)
	m := manifest.Manifest{Key: key, Version: "1.0.0", Database: &manifest.Database{Migrations: "migrations/", Access: access}}
	mj, _ := json.Marshal(m)
	version := "1.0.0"
	if err := g.Create(&model.Plugin{Key: key, Name: json.RawMessage(`{"en":"x"}`), Status: "enabled", ActiveVersion: &version}).Error; err != nil {
		t.Fatal(err)
	}
	if err := g.Create(&model.PluginVersion{PluginKey: key, Version: version, Manifest: mj, ManifestHash: "x", PackageSha256: "x",
		SignatureStatus: "unsigned"}).Error; err != nil {
		t.Fatal(err)
	}
	if !grant {
		return
	}
	scope, _ := json.Marshal(dbschema.AccessScope(access))
	if err := g.Create(&model.PluginPermissionGrant{PluginKey: key, Permission: dbschema.AccessPermission, Scope: scope,
		Status: "granted", PluginVersion: version, ManifestHash: "x"}).Error; err != nil {
		t.Fatal(err)
	}
}

// scripts is a migrations directory.
func scripts(files ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(files); i += 2 {
		fsys[files[i]] = &fstest.MapFile{Data: []byte(files[i+1])}
	}
	return fsys
}

// queryErr runs q on p and returns its error.
func queryErr(p *store.DB, q string) error {
	_, err := p.SQL.ExecContext(context.Background(), q)
	return err
}
