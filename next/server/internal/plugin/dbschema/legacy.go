package dbschema

// Transitional: the conversion of what earlier cores left in a database -
// a login role per plugin (cores up to 0.1.303, CONTRACTS §115) and a
// schema plg_<key> per plugin with its migration records in
// plugin_migrations (cores up to 0.1.303) - to the layout of CONTRACTS
// §130. Everything in this file, its calls (ConvertLegacy at start,
// convertLegacy in Ensure and DropTx, adoptRelocated in Migrate) and
// legacy_test.go go in the first core version after every database has
// started under core 0.1.304 or later: no role in pg_roles carries the
// comment "sub2api plugin role; database=<database>; ..." and no schema
// named plg_<key> is left.

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// legacyName is the name earlier cores gave a plugin's role and schema.
func legacyName(pluginKey string) string { return "plg_" + pluginKey }

// relocatedChecksum marks a migration record relocate carried over: the
// script was rewritten for the new layout, and the first migration records
// its new checksum without running it (adoptRelocated).
const relocatedChecksum = "relocated"

// ConvertLegacy converts, once at start, what earlier cores left for every
// plugin, installed or not (convertLegacy). A plugin whose conversion fails
// is logged and left for its next Ensure or the next start; neither the
// start nor the other plugins wait for it.
func (m *Manager) ConvertLegacy(ctx context.Context) {
	g := store.G(ctx, m.db.Gorm)
	var names []string
	err := g.Raw("(?) UNION (?) ORDER BY 1",
		g.Table("pg_roles").Select("rolname").Where("rolname LIKE ?", `plg\_%`),
		g.Table("pg_namespace").Select("nspname").Where("nspname LIKE ?", `plg\_%`)).Pluck("rolname", &names).Error
	if err != nil {
		m.log.Error("list legacy plugin database roles and schemas", "err", err)
		return
	}
	for _, name := range names {
		key := strings.TrimPrefix(name, "plg_")
		if validKey(key) != nil {
			continue
		}
		err := m.db.Tx(ctx, func(tx *gorm.DB) error {
			if err := lockPlugin(ctx, tx, key); err != nil {
				return err
			}
			_, err := m.convertLegacy(ctx, tx, key)
			return err
		})
		if err != nil {
			m.log.Error("convert the legacy database layout of a plugin", "plugin", key, "err", err)
		}
	}
}

// convertLegacy converts what earlier cores left for plugin pluginKey, in
// tx (which holds the plugin's locks): its role (convertRole), then its
// schema (relocate). A role that still has sessions is left in place
// (left), and so is its schema, which then fails the call: the plugin's
// tables cannot move yet.
func (m *Manager) convertLegacy(ctx context.Context, tx *gorm.DB, pluginKey string) (left bool, err error) {
	if left, err = m.convertRole(ctx, tx, pluginKey); err != nil {
		return left, err
	}
	g := store.G(ctx, tx)
	old := legacyName(pluginKey)
	exists, err := gormpg.Exists(g, g.Table("pg_namespace").Where("nspname = ?", old))
	if err != nil || !exists {
		return left, err
	}
	if left {
		return true, fmt.Errorf("schema %s cannot move while its legacy role still has sessions", old)
	}
	return false, m.relocate(ctx, tx, pluginKey)
}

// relocate moves the plugin's schema plg_<key> into the core's schema, in
// tx: every relation and constraint of it takes the plugin's prefix
// (table "items" becomes "plg_<key>__items", its index "items_pkey"
// "plg_<key>__items_pkey"), the relations move (indexes and the sequences
// of columns with their tables), the plugin's records in plugin_migrations
// move to its records table (relocatedChecksum), and the empty schema is
// dropped. The relations stay owned by the core until Ensure hands them to
// the plugin's identity. Whatever cannot move (a function, a type) keeps
// the schema from being dropped and fails the whole relocation.
func (m *Manager) relocate(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	old := legacyName(pluginKey)
	oid, sid := store.Ident(old), store.Ident(m.schema)
	prefix := manifest.TablePrefix(pluginKey)
	var rels []string
	if err := g.Table("pg_class").Where("relnamespace = ?::regnamespace AND relkind IN ('r', 'p', 'i', 'I', 'S', 'v', 'm', 'f')", old).
		Order("relname").Pluck("relname", &rels).Error; err != nil {
		return err
	}
	for _, name := range rels {
		if err := legacyRename(prefix, name); err != nil {
			return err
		}
		if err := store.DDL(g, "ALTER TABLE %s.%s RENAME TO %s", oid, store.Ident(name), store.Ident(prefix+name)); err != nil {
			return fmt.Errorf("rename %s.%s: %w", old, name, err)
		}
	}
	// Constraints backed by an index were renamed with it.
	var cons []legacyConstraint
	if err := g.Table("pg_constraint AS k").Select("c.relname AS rel, k.conname AS name").
		Joins("JOIN pg_class AS c ON c.oid = k.conrelid").
		Where("k.connamespace = ?::regnamespace AND k.contype NOT IN ('p', 'u', 'x')", old).
		Order("c.relname, k.conname").Scan(&cons).Error; err != nil {
		return err
	}
	for _, c := range cons {
		to := legacyConstraintName(prefix, c.Name)
		if err := legacyRename("", to); err != nil {
			return err
		}
		if err := store.DDL(g, "ALTER TABLE %s.%s RENAME CONSTRAINT %s TO %s", oid, store.Ident(c.Rel), store.Ident(c.Name), store.Ident(to)); err != nil {
			return fmt.Errorf("rename constraint %s of %s.%s: %w", c.Name, old, c.Rel, err)
		}
	}
	var top []string
	ownedSeq := g.Table("pg_depend AS d").Select("1").
		Where("d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')")
	if err := g.Table("pg_class AS c").Where("c.relnamespace = ?::regnamespace AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND NOT EXISTS (?)", old, ownedSeq).
		Order("c.relname").Pluck("c.relname", &top).Error; err != nil {
		return err
	}
	for _, name := range top {
		if err := store.DDL(g, "ALTER TABLE %s.%s SET SCHEMA %s", oid, store.Ident(name), sid); err != nil {
			return fmt.Errorf("move %s.%s to schema %s: %w", old, name, m.schema, err)
		}
	}
	if err := relocateRecords(ctx, tx, m.schema, pluginKey); err != nil {
		return err
	}
	if err := store.DDL(g, "DROP SCHEMA %s", oid); err != nil {
		return fmt.Errorf("schema %s holds objects that cannot move to schema %s (only tables, views, sequences and indexes can): %w", old, m.schema, err)
	}
	m.log.Info("moved the plugin's tables into the core's schema", "plugin", pluginKey, "from", old, "to", m.schema, "relations", len(rels))
	return nil
}

// legacyRename fails when prefix+name would be longer than PostgreSQL's
// identifiers, which it would cut short.
func legacyRename(prefix, name string) error {
	if len(prefix+name) > 63 {
		return fmt.Errorf("%s%s is longer than 63 bytes", prefix, name)
	}
	return nil
}

// legacyConstraintName is the name a constraint gets in the core's schema,
// the one the rewritten scripts give it: the prefix in front, but after
// the "fk_" of a foreign key named by GORM's rule fk_<table>_<field>, so
// that it keeps that rule ("fk_orders_user" of table "orders" becomes
// "fk_plg_<key>__orders_user").
func legacyConstraintName(prefix, name string) string {
	if rest, ok := strings.CutPrefix(name, "fk_"); ok {
		return "fk_" + prefix + rest
	}
	return prefix + name
}

// legacyConstraint is a constraint relocate renames.
type legacyConstraint struct {
	Rel  string `gorm:"column:rel"`
	Name string `gorm:"column:name"`
}

// legacyRecord is a plugin_migrations row of a plugin's SQL migration.
type legacyRecord struct {
	MigrationID string    `gorm:"column:migration_id"`
	AppliedAt   time.Time `gorm:"column:applied_at"`
}

// relocateRecords moves the plugin's SQL migration records from
// plugin_migrations to its records table, created by the core (Ensure
// hands it to the identity with the rest). The core's records of the
// plugin's data migrations ("data:..." ids) stay.
func relocateRecords(ctx context.Context, tx *gorm.DB, schema, pluginKey string) error {
	g := store.G(ctx, tx)
	sql := dataMigrations(g, pluginKey).Where("migration_id LIKE ?", "%.sql")
	var recs []legacyRecord
	if err := sql.Select("migration_id, applied_at").Scan(&recs).Error; err != nil {
		return err
	}
	t := Tracker{Schema: schema, Table: recordsTable(pluginKey)}
	if err := t.Ensure(ctx, tx); err != nil {
		return err
	}
	for _, r := range recs {
		if err := g.Table(t.table()).Clauses(clause.OnConflict{DoNothing: true}).
			Create(&migrationRecord{MigrationID: r.MigrationID, Checksum: relocatedChecksum, AppliedAt: r.AppliedAt}).Error; err != nil {
			return err
		}
	}
	return dataMigrations(g, pluginKey).Where("migration_id LIKE ?", "%.sql").Delete(nil).Error
}

// adoptRelocated records the checksum of each file of fsys whose record
// relocate carried over, so that it is not run again.
func (m *Manager) adoptRelocated(ctx context.Context, pluginKey string, fsys fs.FS) error {
	g := store.G(ctx, m.db.Gorm)
	table := store.QuoteIdent(m.schema, recordsTable(pluginKey))
	if exists, err := gormpg.RelationExists(g, table); err != nil || !exists {
		return err
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if err := g.Table(table).Where("migration_id = ? AND checksum = ?", name, relocatedChecksum).
			Update("checksum", store.MigrationChecksum(body)).Error; err != nil {
			return err
		}
	}
	return nil
}

// roleMarker is the comment earlier cores put on the plugin roles
// they created. Roles are shared by every database of the PostgreSQL
// cluster, so it names the database: a same-named role of another database
// (another deployment) is not this core's.
func roleMarker(ctx context.Context, q *gorm.DB, pluginKey string) (string, error) {
	dbname, err := store.CurrentDatabase(store.G(ctx, q))
	if err != nil {
		return "", err
	}
	return "sub2api plugin role; database=" + dbname + "; plugin=" + pluginKey, nil
}

// roleOwned reports whether a role named role exists and belongs to this
// plugin of this database: it carries the marker, or (roles created before
// the marker) it owns the plugin's schema here.
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

// convertRole takes back what the legacy role plg_<key> owns in this
// database and drops it, in tx (which holds the plugin's locks). It needs
// the privileges of the role, which the core has: the core created the
// role and made itself a member. Nothing happens without such a role (a
// new database, a restore without owners). A role that still has sessions
// - an old plugin process on a node not restarted yet - is left in place
// (left = true), logged once, and converted at a later call; dropping it
// would cut that process off its own tables. A same-named role this core
// did not create is not touched.
func (m *Manager) convertRole(ctx context.Context, tx *gorm.DB, pluginKey string) (left bool, err error) {
	role := legacyName(pluginKey)
	marker, err := roleMarker(ctx, tx, pluginKey)
	if err != nil {
		return false, err
	}
	owned, err := roleOwned(ctx, tx, role, role, marker)
	if err != nil || !owned {
		return false, err
	}
	g := store.G(ctx, tx)
	busy, err := gormpg.Exists(g, g.Table("pg_stat_activity").Where("usename = ?", role))
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	logged := m.busy[role]
	m.busy[role] = busy
	m.mu.Unlock()
	if busy {
		if !logged {
			m.log.Warn("legacy plugin database role still has sessions; it is converted once they end", "role", role)
		}
		return true, nil
	}
	// DROP OWNED revokes grants on objects several roles may share.
	if err := store.XactLock(ctx, tx, aclLock); err != nil {
		return false, err
	}
	rid := store.Ident(role)
	for _, ddl := range []string{"REASSIGN OWNED BY %s TO CURRENT_USER", "DROP OWNED BY %s", "DROP ROLE %s"} {
		if err := store.DDL(g, ddl, rid); err != nil {
			return false, fmt.Errorf("convert legacy role %s: %w", role, err)
		}
	}
	m.log.Info("converted legacy plugin database role", "role", role)
	return false, nil
}
