// Package dbschema owns plugin database schemas (plg_<key>), the optional
// per-plugin login role, plugin SQL migrations and the restricted DSN handed
// to plugins.
package dbschema

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,29}$`)

// SchemaName returns "plg_<key>".
func SchemaName(pluginKey string) string { return "plg_" + pluginKey }

// Status describes the database isolation of one plugin.
type Status struct {
	Schema       string `json:"schema"`
	Role         string `json:"role,omitempty"`
	RoleIsolated bool   `json:"role_isolated"`
}

// Manager implements core.PluginSchemaManager and the runtime-internal
// schema API.
type Manager struct {
	db            *store.DB
	databaseURL   string
	masterKey     []byte
	roleIsolation bool

	mu     sync.Mutex
	status map[string]Status
}

// New creates the manager. databaseURL is the host DSN (config
// DatabaseURL); masterKey derives per-plugin role passwords; roleIsolation
// mirrors config Plugins.DBRoleIsolation.
func New(db *store.DB, databaseURL string, masterKey []byte, roleIsolation bool) *Manager {
	return &Manager{db: db, databaseURL: databaseURL, masterKey: masterKey, roleIsolation: roleIsolation, status: map[string]Status{}}
}

var _ core.PluginSchemaManager = (*Manager)(nil)

// RolePassword derives the plugin role password (never stored).
func (m *Manager) RolePassword(pluginKey string) string {
	mac := hmac.New(sha256.New, m.masterKey)
	mac.Write([]byte("plugin-db:" + pluginKey))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// canCreateRole reports whether the current connection may create roles.
func (m *Manager) canCreateRole(ctx context.Context) (bool, error) {
	var ok bool
	err := store.G(ctx, m.db.Gorm).Table("pg_roles").Select("rolcreaterole OR rolsuper").Where("rolname = current_user").Row().Scan(&ok)
	return ok, err
}

// The statements below read the system catalogs (pg_roles and the other
// views, by the builder) and change roles and schemas (store.DDL, which
// quotes the names and literals into the text). A one-value read uses
// Row().Scan, which, like pgx's QueryRow, fails on no row.

// Error codes of the database isolation (audit 2026-10-09 P1-7). Role
// isolation is never downgraded silently: a plugin that needs db.schema is
// refused instead.
var (
	// ErrIsolationUnavailable: role isolation is on but the core's database
	// role may not create roles (CREATEROLE), so the plugin would run with
	// the core's own credentials.
	ErrIsolationUnavailable = core.NewError(http.StatusConflict, "plugin_db_isolation_unavailable",
		"the database role of the core cannot create plugin roles (CREATEROLE); grant it, or set SUB2API_PLUGIN_DB_ROLE_ISOLATION=false to run plugin databases without role isolation")
	// ErrRoleConflict: a role named plg_<key> exists that this core did not
	// create for this database; it is not taken over.
	ErrRoleConflict = core.NewError(http.StatusConflict, "plugin_db_role_conflict",
		"a database role with the plugin's role name already exists and was not created by this core for this database")
)

// CheckIsolation reports whether plugin schemas get their own role: nil when
// role isolation is off by configuration (an explicit operator choice) or
// the core may create roles, ErrIsolationUnavailable otherwise.
func (m *Manager) CheckIsolation(ctx context.Context) error {
	if !m.roleIsolation {
		return nil
	}
	ok, err := m.canCreateRole(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIsolationUnavailable
	}
	return nil
}

// roleMarker is the comment Ensure puts on the roles it creates. Roles are
// shared by every database of the PostgreSQL cluster, so it names this
// database: another database (another deployment) using the same plugin key
// does not own the role.
func roleMarker(ctx context.Context, q *gorm.DB, pluginKey string) (string, error) {
	dbname, err := store.CurrentDatabase(store.G(ctx, q))
	if err != nil {
		return "", err
	}
	return "sub2api plugin role; database=" + dbname + "; plugin=" + pluginKey, nil
}

// roleOwned reports whether the existing role belongs to this plugin of this
// database: it carries the marker, or (roles created before the marker) it
// owns the plugin's schema here.
func roleOwned(ctx context.Context, q *gorm.DB, role, schema, marker string) (bool, error) {
	var owned bool
	err := roleOwnedQuery(store.G(ctx, q), role, schema, marker).Row().Scan(&owned)
	if store.IsNoRows(err) {
		return false, nil
	}
	return owned, err
}

func roleOwnedQuery(g *gorm.DB, role, schema, marker string) *gorm.DB {
	ownsSchema := g.Table("pg_namespace AS n").Select("1").Where("n.nspname = ? AND n.nspowner = r.oid", schema)
	return g.Table("pg_roles AS r").
		Select("COALESCE(shobj_description(r.oid, 'pg_authid') = ?, false) OR EXISTS (?)", marker, ownsSchema).
		Where("r.rolname = ?", role)
}

// grantMigrationFuncsSQL lets a role run the core functions a plugin role
// runs for its own migration bookkeeping (0003 migration).
const grantMigrationFuncsSQL = "GRANT EXECUTE ON FUNCTION public.plugin_migrations_applied(varchar), public.plugin_migration_record(varchar, varchar, varchar) TO %s"

// sharedACLLock (store.XactLock) serializes changes to the ACL of the
// migration functions (grantMigrationFuncsSQL). Every plugin role is granted
// on the same two pg_proc rows, so two plugins' Ensure (or Drop) at once -
// every node starts its plugins together - would otherwise fail with "tuple
// concurrently updated". It is always taken after a plugin's own lock, never
// before.
const sharedACLLock = "plugin-migration-function-acl"

// schemaLock (store.XactLock) serializes Ensure and Drop of one plugin's
// schema across the nodes.
func schemaLock(pluginKey string) string { return "plugin-schema:" + pluginKey }

// grantMigrationFuncs lets role run the migration functions. Most Ensure
// calls find the grant in place and write nothing.
func grantMigrationFuncs(ctx context.Context, tx *gorm.DB, role string) error {
	g := store.G(ctx, tx)
	granted := func() (bool, error) {
		var ok bool
		err := g.Table("pg_roles").Select(`has_function_privilege(oid, 'public.plugin_migrations_applied(varchar)', 'EXECUTE')
			AND has_function_privilege(oid, 'public.plugin_migration_record(varchar, varchar, varchar)', 'EXECUTE')`).
			Where("rolname = ?", role).Row().Scan(&ok)
		return ok, err
	}
	if ok, err := granted(); err != nil || ok {
		return err
	}
	if err := store.XactLock(ctx, tx, sharedACLLock); err != nil {
		return err
	}
	if ok, err := granted(); err != nil || ok {
		return err
	}
	if err := store.DDL(g, grantMigrationFuncsSQL, store.Ident(role)); err != nil {
		return fmt.Errorf("grant the migration functions to %s: %w", role, err)
	}
	return nil
}

// Ensure creates the schema and, unless role isolation is off by
// configuration, the isolated login role. It is idempotent and safe to call
// on every node. Without the right to create roles it fails with
// ErrIsolationUnavailable, and a same-named role it did not create fails
// with ErrRoleConflict.
func (m *Manager) Ensure(ctx context.Context, pluginKey string) (Status, error) {
	if !keyRe.MatchString(pluginKey) {
		return Status{}, fmt.Errorf("invalid plugin key %q", pluginKey)
	}
	schema := SchemaName(pluginKey)
	st := Status{Schema: schema}
	if err := m.CheckIsolation(ctx); err != nil {
		return st, err
	}
	isolate := m.roleIsolation
	sid := store.Ident(schema)
	err := m.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		// Serialize with other nodes ensuring or dropping the same schema.
		if err := store.XactLock(ctx, tx, schemaLock(pluginKey)); err != nil {
			return err
		}
		if !isolate {
			return store.DDL(g, "CREATE SCHEMA IF NOT EXISTS %s", sid)
		}
		role := schema
		rid := store.Ident(role)
		pw := store.Literal(m.RolePassword(pluginKey))
		marker, err := roleMarker(ctx, tx, pluginKey)
		if err != nil {
			return err
		}
		exists, err := gormpg.Exists(g, g.Table("pg_roles").Where("rolname = ?", role))
		if err != nil {
			return err
		}
		if exists {
			owned, err := roleOwned(ctx, tx, role, schema, marker)
			if err != nil {
				return err
			}
			if !owned {
				return ErrRoleConflict
			}
		}
		if err := store.DDL(g, "CREATE SCHEMA IF NOT EXISTS %s", sid); err != nil {
			return err
		}
		if exists {
			// Name only the attributes to take away: on PostgreSQL 16+ a
			// non-superuser host (CREATEROLE + ADMIN OPTION, e.g. Supabase)
			// may not mention SUPERUSER, REPLICATION or BYPASSRLS at all,
			// not even to restate NO.
			var super, repl, bypass, createdb, createrole bool
			if err := g.Table("pg_roles").Select("rolsuper, rolreplication, rolbypassrls, rolcreatedb, rolcreaterole").
				Where("rolname = ?", role).Row().Scan(&super, &repl, &bypass, &createdb, &createrole); err != nil {
				return err
			}
			if err := store.DDL(g, "ALTER ROLE %s WITH LOGIN CONNECTION LIMIT 20 PASSWORD %s", rid, pw); err != nil {
				return fmt.Errorf("alter role %s: %w", role, err)
			}
			for _, a := range []struct {
				set   bool
				alter string
			}{
				{super, "ALTER ROLE %s WITH NOSUPERUSER"}, {createdb, "ALTER ROLE %s WITH NOCREATEDB"},
				{createrole, "ALTER ROLE %s WITH NOCREATEROLE"}, {repl, "ALTER ROLE %s WITH NOREPLICATION"},
				{bypass, "ALTER ROLE %s WITH NOBYPASSRLS"},
			} {
				if !a.set {
					continue
				}
				if err := store.DDL(g, a.alter, rid); err != nil {
					return fmt.Errorf("alter role %s: %w", role, err)
				}
			}
		} else {
			if err := store.DDL(g, "CREATE ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 20 PASSWORD %s", rid, pw); err != nil {
				return err
			}
		}
		if err := store.DDL(g, "COMMENT ON ROLE %s IS %s", rid, store.Literal(marker)); err != nil {
			return err
		}
		// Host membership lets a non-superuser host hand the schema to the role
		// and DROP OWNED BY it on purge (PG16+ does not grant SET on created
		// roles by default). Migrations never use SET ROLE.
		var verNum int
		if err := g.Table("pg_settings").Select("setting::int").Where("name = 'server_version_num'").Row().Scan(&verNum); err != nil {
			return err
		}
		priv := "MEMBER"
		if verNum >= 160000 {
			priv = "SET"
		}
		var member bool
		if err := g.Table("pg_roles").Select("pg_has_role(current_user, oid, ?)", priv).Where("rolname = ?", role).Row().Scan(&member); err != nil {
			return err
		}
		if !member {
			if err := store.DDL(g, "GRANT %s TO CURRENT_USER", rid); err != nil {
				return err
			}
		}
		for _, s := range []struct {
			ddl  string
			args []store.DDLArg
		}{
			{"ALTER SCHEMA %s OWNER TO %s", []store.DDLArg{sid, rid}},
			{"REVOKE ALL ON SCHEMA %s FROM PUBLIC", []store.DDLArg{sid}},
			{"GRANT USAGE, CREATE ON SCHEMA %s TO %s", []store.DDLArg{sid, rid}},
			{"ALTER ROLE %s SET search_path TO %s", []store.DDLArg{rid, sid}},
		} {
			if err := store.DDL(g, s.ddl, s.args...); err != nil {
				return fmt.Errorf("%s (schema %s): %w", s.ddl, schema, err)
			}
		}
		if err := grantMigrationFuncs(ctx, tx, role); err != nil {
			return err
		}
		st.Role = role
		st.RoleIsolated = true
		return nil
	})
	if err != nil {
		return st, fmt.Errorf("ensure schema %s: %w", schema, err)
	}
	m.mu.Lock()
	m.status[pluginKey] = st
	m.mu.Unlock()
	return st, nil
}

// Status returns the last known isolation status, ensuring it when unknown.
func (m *Manager) Status(ctx context.Context, pluginKey string) (Status, error) {
	m.mu.Lock()
	st, ok := m.status[pluginKey]
	m.mu.Unlock()
	if ok {
		return st, nil
	}
	return m.Ensure(ctx, pluginKey)
}

// Migrate ensures the schema and applies the plugin's SQL migrations from
// fsys (the package migrations directory). Returns applied file names.
//
// With role isolation the files run on a connection logged in as the plugin
// role, so a migration cannot escalate with RESET ROLE; bookkeeping goes
// through the definer functions of migration 0003. Only when the operator
// turned role isolation off (SUB2API_PLUGIN_DB_ROLE_ISOLATION=false) do they
// run as the host role, as the install review warns; a host that merely
// lacks CREATEROLE fails in Ensure instead.
func (m *Manager) Migrate(ctx context.Context, pluginKey string, fsys fs.FS) ([]string, error) {
	st, err := m.Ensure(ctx, pluginKey)
	if err != nil {
		return nil, err
	}
	if fsys == nil {
		return nil, nil
	}
	// Plugin scripts are idempotent by contract: a script whose content
	// changed after it was applied runs again and its record is updated.
	opt := store.MigrateOptions{LockKey: store.PluginMigrationLockKey(pluginKey), SearchPath: st.Schema, RerunChanged: true}
	if !st.RoleIsolated {
		return store.Migrate(ctx, m.db, fsys, Tracker{PluginKey: pluginKey}, opt)
	}
	dsn, err := rewriteDSN(m.databaseURL, st.Role, m.RolePassword(pluginKey), st.Schema)
	if err != nil {
		return nil, err
	}
	pool, err := store.OpenSized(ctx, dsn, 1)
	if err != nil {
		return nil, fmt.Errorf("connect as plugin role: %w", err)
	}
	defer pool.Close()
	return store.Migrate(ctx, pool, fsys, Tracker{PluginKey: pluginKey, AsPluginRole: true}, opt)
}

// Pending reports whether any of ids has not been applied yet.
func (m *Manager) Pending(ctx context.Context, pluginKey string, ids []string) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	var n int64
	err := pluginMigrations(store.G(ctx, m.db.Gorm), pluginKey).Where("migration_id = ANY(?)", store.Strings(ids)).Count(&n).Error
	return n < int64(len(ids)), err
}

// pluginMigrations filters the migration records of a plugin.
// plugin_migrations belongs to store's migration runner and has no model.
func pluginMigrations(g *gorm.DB, pluginKey string) *gorm.DB {
	return g.Table("plugin_migrations").Where("plugin_key = ?", pluginKey)
}

func dropMigrationRecords(g *gorm.DB, pluginKey string) *gorm.DB {
	return pluginMigrations(g, pluginKey).Delete(nil)
}

// DSN returns the connection string a plugin should use for its schema.
func (m *Manager) DSN(ctx context.Context, pluginKey string) (dsn string, st Status, err error) {
	st, err = m.Status(ctx, pluginKey)
	if err != nil {
		return "", st, err
	}
	user, pass := "", ""
	if st.RoleIsolated {
		user, pass = st.Role, m.RolePassword(pluginKey)
	}
	dsn, err = rewriteDSN(m.databaseURL, user, pass, st.Schema)
	return dsn, st, err
}

// Drop removes the schema, the role and the migration records.
func (m *Manager) Drop(ctx context.Context, pluginKey string) error {
	return m.db.Tx(ctx, func(tx *gorm.DB) error { return m.DropTx(ctx, tx, pluginKey) })
}

// DropTx holds both schema and migration locks until the lifecycle transaction
// commits. Invalidate the local DSN cache even on rollback; Ensure rebuilds it.
func (m *Manager) DropTx(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	if !keyRe.MatchString(pluginKey) {
		return fmt.Errorf("invalid plugin key %q", pluginKey)
	}
	schema := SchemaName(pluginKey)
	sid := store.Ident(schema)
	g := store.G(ctx, tx)
	err := func() error {
		if err := store.XactLock(ctx, tx, schemaLock(pluginKey)); err != nil {
			return err
		}
		if err := store.XactLockID(ctx, tx, store.PluginMigrationLockKey(pluginKey)); err != nil {
			return err
		}
		// Decide before the schema goes: a role that owns it is ours.
		marker, err := roleMarker(ctx, tx, pluginKey)
		if err != nil {
			return err
		}
		owned, err := roleOwned(ctx, tx, schema, schema, marker)
		if err != nil {
			return err
		}
		if err := store.DDL(g, "DROP SCHEMA IF EXISTS %s CASCADE", sid); err != nil {
			return err
		}
		if err := dropMigrationRecords(g, pluginKey).Error; err != nil {
			return err
		}
		// A same-named role this core did not create (another database of
		// the cluster, an operator's role) is left alone.
		if owned {
			// DROP OWNED revokes the role's grant on the migration functions.
			if err := store.XactLock(ctx, tx, sharedACLLock); err != nil {
				return err
			}
			if err := store.DDL(g, "DROP OWNED BY %s", sid); err != nil {
				return err
			}
			if err := store.DDL(g, "DROP ROLE %s", sid); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return fmt.Errorf("drop plugin schema %s: %w", schema, err)
	}
	m.mu.Lock()
	delete(m.status, pluginKey)
	m.mu.Unlock()
	return nil
}

// Tracker records plugin migrations in public.plugin_migrations. With
// AsPluginRole the queries run as the plugin role and go through the
// definer functions, which only accept the role's own key.
type Tracker struct {
	PluginKey    string
	AsPluginRole bool
}

func (Tracker) Ensure(context.Context, *gorm.DB) error { return nil }

// migrationRecord is a row Tracker.Applied reads.
type migrationRecord struct {
	MigrationID string `gorm:"column:migration_id"`
	Checksum    string `gorm:"column:checksum"`
}

func (t Tracker) Applied(ctx context.Context, q *gorm.DB) (map[string]string, error) {
	var rows []migrationRecord
	if err := t.appliedQuery(store.G(ctx, q)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.MigrationID] = r.Checksum
	}
	return out, nil
}

// appliedQuery reads the plugin's records, schema-qualified: the
// connection's search_path is the plugin schema. The plugin role may only
// read them through the definer function.
func (t Tracker) appliedQuery(g *gorm.DB) *gorm.DB {
	if t.AsPluginRole {
		return g.Table("public.plugin_migrations_applied(?) AS m", t.PluginKey).Select("migration_id, checksum")
	}
	return g.Table("public.plugin_migrations").Select("migration_id, checksum").Where("plugin_key = ?", t.PluginKey)
}

func (t Tracker) Record(ctx context.Context, tx *gorm.DB, id, checksum string) error {
	return t.recordQuery(store.G(ctx, tx), id, checksum).Error
}

// recordQuery records an applied migration: as the plugin role through the
// definer function (void, hence Exec), else an upsert.
func (t Tracker) recordQuery(g *gorm.DB, id, checksum string) *gorm.DB {
	if t.AsPluginRole {
		return g.Exec(`SELECT public.plugin_migration_record(?, ?, ?)`, t.PluginKey, id, checksum)
	}
	return g.Table("public.plugin_migrations").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "plugin_key"}, {Name: "migration_id"}},
		DoUpdates: append(clause.AssignmentColumns([]string{"checksum"}),
			clause.Assignment{Column: clause.Column{Name: "applied_at"}, Value: gorm.Expr("now()")}),
	}).Create(map[string]any{"plugin_key": t.PluginKey, "migration_id": id, "checksum": checksum})
}

// rewriteDSN replaces the credentials of dsn (URL or key=value form) and pins
// search_path to schema. Empty user keeps the host credentials. The host's
// pgxpool parameters (pool_max_conns, ...) stay, as they did up to 0.1.288:
// every opener takes them out (gormpg.ParseConfig, for the host's own
// connections as the plugin role and the SDK's pgdb.Open, which sizes a
// plugin's pool by its options alone).
func rewriteDSN(dsn, user, pass, schema string) (string, error) {
	opt := "-c search_path=" + schema
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse database url: %w", err)
		}
		if user != "" {
			u.User = url.UserPassword(user, pass)
		}
		q := u.Query()
		q.Set("options", opt)
		// libpq does not decode "+" as a space in URLs.
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
		return u.String(), nil
	}
	// key=value form: later keys override earlier ones.
	var b strings.Builder
	b.WriteString(strings.TrimSpace(dsn))
	if user != "" {
		fmt.Fprintf(&b, " user=%s password=%s", kvQuote(user), kvQuote(pass))
	}
	fmt.Fprintf(&b, " options=%s", kvQuote(opt))
	return strings.TrimSpace(b.String()), nil
}

func kvQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
