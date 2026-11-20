package dbschema

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// What a plugin's identity may own (CONTRACTS §130.3): relations and types
// of the plugin's prefix in the core's schema, and what PostgreSQL makes
// with them (indexes, sequences of columns, TOAST tables, row, array and
// multirange types). Anything else is a stray: an object without the
// prefix, an object of any other catalog (a function, a procedure, an
// operator, a collation, a text search configuration, statistics, ...)
// and a trigger, rule or policy on the plugin's relations. An operator or
// function in the core's schema would be picked by the core's own queries
// (CVE-2018-1058); the others have no use the plugins need. A migration
// file that leaves a stray fails (prefixCheck), and removeStrays drops
// whatever got past it (a file that commits halfway, a plugin process
// adding a trigger to its own table).

// stray is an object the identity may not own.
type stray struct {
	// Kind is the relkind of a relation, "constraint" (one with an index:
	// primary key, unique, exclusion), "type", "domain", "trigger", "rule",
	// "policy", or the catalog of another object.
	Kind  string `gorm:"column:kind"`
	Table string `gorm:"column:tbl"` // the relation of a constraint, trigger, rule or policy
	Name  string `gorm:"column:name"`
}

// relkinds names the relation kinds of pg_class.
var relkinds = map[string]string{"r": "table", "p": "partitioned table", "i": "index", "I": "partitioned index", "S": "sequence",
	"v": "view", "m": "materialized view", "c": "type", "f": "foreign table"}

func (s stray) String() string {
	switch {
	case s.Table != "":
		return s.Kind + " " + s.Name + " on " + s.Table
	case relkinds[s.Kind] != "":
		return relkinds[s.Kind] + " " + s.Name
	case strings.HasPrefix(s.Kind, "pg_"):
		return s.Name // pg_describe_object names the kind
	}
	return s.Kind + " " + s.Name
}

// strayObjects selects the strays of role in schema for prefix, as stray
// rows. Every object a role owns has a pg_shdepend row except the ones
// that go with another object (an index, a row or array type), which the
// owner columns of pg_class and pg_type list.
func strayObjects(g *gorm.DB, schema, prefix, role string) *gorm.DB {
	others := g.Table("pg_shdepend AS d").
		Select("d.classid::regclass::text AS kind, '' AS tbl, pg_describe_object(d.classid, d.objid, d.objsubid) AS name").
		Where(`d.dbid = (SELECT oid FROM pg_database WHERE datname = current_database()) AND d.deptype = 'o'
			AND d.refclassid = 'pg_authid'::regclass AND d.refobjid = ?::regrole
			AND d.classid NOT IN ('pg_class'::regclass, 'pg_type'::regclass)`, role)
	backs := g.Table("pg_constraint AS k").Select("1").Where("k.conindid = c.oid AND k.contype IN ('p', 'u', 'x')")
	rels := g.Table("pg_class AS c").Select("c.relkind::text, '', c.relname").
		Where("c.relnamespace = ?::regnamespace AND c.relowner = ?::regrole AND NOT starts_with(c.relname, ?) AND NOT EXISTS (?)", schema, role, prefix, backs)
	// An index of a constraint goes with the constraint.
	constraints := g.Table("pg_constraint AS k").Select("'constraint', c.relname, k.conname").
		Joins("JOIN pg_class AS i ON i.oid = k.conindid").
		Joins("JOIN pg_class AS c ON c.oid = k.conrelid").
		Where("k.contype IN ('p', 'u', 'x') AND i.relnamespace = ?::regnamespace AND i.relowner = ?::regrole AND NOT starts_with(i.relname, ?)", schema, role, prefix)
	arrayOf := g.Table("pg_type AS e").Select("1").Where("e.typarray = t.oid")
	types := g.Table("pg_type AS t").Select("CASE t.typtype WHEN 'd' THEN 'domain' ELSE 'type' END, '', t.typname").
		Where(`t.typnamespace = ?::regnamespace AND t.typowner = ?::regrole AND t.typrelid = 0 AND t.typtype <> 'm'
			AND NOT EXISTS (?) AND NOT starts_with(t.typname, ?)`, schema, role, arrayOf, prefix)
	triggers := g.Table("pg_trigger AS tg").Select("'trigger', c.relname, tg.tgname").
		Joins("JOIN pg_class AS c ON c.oid = tg.tgrelid").
		Where("c.relnamespace = ?::regnamespace AND c.relowner = ?::regrole AND NOT tg.tgisinternal", schema, role)
	rules := g.Table("pg_rewrite AS rw").Select("'rule', c.relname, rw.rulename").
		Joins("JOIN pg_class AS c ON c.oid = rw.ev_class").
		Where("c.relnamespace = ?::regnamespace AND c.relowner = ?::regrole AND rw.rulename <> '_RETURN'", schema, role)
	policies := g.Table("pg_policy AS pol").Select("'policy', c.relname, pol.polname").
		Joins("JOIN pg_class AS c ON c.oid = pol.polrelid").
		Where("c.relnamespace = ?::regnamespace AND c.relowner = ?::regrole", schema, role)
	return g.Raw("(?) UNION ALL (?) UNION ALL (?) UNION ALL (?) UNION ALL (?) UNION ALL (?) UNION ALL (?)",
		others, rels, constraints, types, triggers, rules, policies)
}

// strays lists the strays of plugin pluginKey's identity.
func (m *Manager) strays(g *gorm.DB, pluginKey string) ([]stray, error) {
	var out []stray
	err := strayObjects(g, m.schema, manifest.TablePrefix(pluginKey), m.Identity(pluginKey)).Scan(&out).Error
	return out, err
}

// prefixCheck fails a migration file that leaves a stray. It runs in the
// file's transaction, as the identity.
func (m *Manager) prefixCheck(pluginKey string) func(context.Context, *gorm.DB) error {
	return func(ctx context.Context, tx *gorm.DB) error {
		found, err := m.strays(store.G(ctx, tx), pluginKey)
		if err != nil || len(found) == 0 {
			return err
		}
		return fmt.Errorf("plugin migrations may create only tables, indexes, sequences, views and types named %s...: %s",
			manifest.TablePrefix(pluginKey), joinStrays(found))
	}
}

func joinStrays(found []stray) string {
	names := make([]string, len(found))
	for i, s := range found {
		names[i] = s.String()
	}
	return strings.Join(names, ", ")
}

// removeStrays drops the strays of plugin pluginKey's identity, in tx
// (which holds the plugin's locks or its migration lock). The ones on or
// part of the plugin's relations go by name: triggers, rules, policies,
// constraints, indexes and sequences of columns. For the rest the plugin's relations
// and types move to the core, DROP OWNED ... CASCADE drops whatever the
// identity still owns (and its grants, which syncAccess gives back) and
// the plugin's objects go back to it: what depends on a stray goes with
// it, a column of a stray type included.
func (m *Manager) removeStrays(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	found, err := m.strays(g, pluginKey)
	if err != nil || len(found) == 0 {
		return err
	}
	if err := store.XactLock(ctx, tx, aclLock); err != nil {
		return err
	}
	sid := store.Ident(m.schema)
	for _, s := range found {
		if err := dropStray(g, sid, s); err != nil {
			return err
		}
	}
	rest, err := m.strays(g, pluginKey)
	if err != nil || len(rest) == 0 {
		m.log.Warn("dropped objects the plugin's database identity may not own", "plugin", pluginKey, "objects", joinStrays(found))
		return err
	}
	owned, err := m.ownedObjects(g, pluginKey, true)
	if err != nil {
		return err
	}
	for _, o := range owned {
		if err := alterOwner(g, o.Kind, sid, store.Ident(o.Name), "CURRENT_USER"); err != nil {
			return err
		}
	}
	if err := store.DDL(g, "DROP OWNED BY %s CASCADE", store.Ident(m.Identity(pluginKey))); err != nil {
		return err
	}
	if err := m.handBack(ctx, tx, pluginKey); err != nil {
		return err
	}
	if rest, err = m.strays(g, pluginKey); err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("the plugin's database identity still owns %s", joinStrays(rest))
	}
	m.log.Warn("dropped objects the plugin's database identity may not own", "plugin", pluginKey, "objects", joinStrays(found))
	return nil
}

// dropStray drops the stray s of schema that goes by name (a trigger,
// rule, policy or constraint, a relation of kind i, I, S, v, m or f); it
// leaves the others to removeStrays.
func dropStray(g *gorm.DB, schema store.Ident, s stray) error {
	name, table := store.Ident(s.Name), store.Ident(s.Table)
	var err error
	switch s.Kind {
	case "trigger":
		err = store.DDL(g, "DROP TRIGGER IF EXISTS %s ON %s.%s CASCADE", name, schema, table)
	case "rule":
		err = store.DDL(g, "DROP RULE IF EXISTS %s ON %s.%s CASCADE", name, schema, table)
	case "policy":
		err = store.DDL(g, "DROP POLICY IF EXISTS %s ON %s.%s CASCADE", name, schema, table)
	case "constraint":
		err = store.DDL(g, "ALTER TABLE %s.%s DROP CONSTRAINT IF EXISTS %s CASCADE", schema, table, name)
	case "i", "I", "S", "v", "m", "f":
		err = dropRelation(g, s.Kind, schema, name)
	}
	if err != nil {
		return fmt.Errorf("drop %s: %w", s, err)
	}
	return nil
}

// codeObjects selects, as stray rows, the objects of prefix in schema that
// run with their owner's rights or act on its behalf, whoever owns them:
// views and materialized views (their queries), foreign tables (the
// owner's user mapping), and the triggers, rules and policies on the
// prefix's relations. An uninstall that keeps the data drops them before
// the core takes the rest (dropCode).
func codeObjects(g *gorm.DB, schema, prefix string) *gorm.DB {
	rels := g.Table("pg_class AS c").Select("c.relkind::text AS kind, '' AS tbl, c.relname AS name").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?) AND c.relkind IN ('v', 'm', 'f')", schema, prefix)
	triggers := g.Table("pg_trigger AS tg").Select("'trigger', c.relname, tg.tgname").
		Joins("JOIN pg_class AS c ON c.oid = tg.tgrelid").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?) AND NOT tg.tgisinternal", schema, prefix)
	rules := g.Table("pg_rewrite AS rw").Select("'rule', c.relname, rw.rulename").
		Joins("JOIN pg_class AS c ON c.oid = rw.ev_class").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?) AND rw.rulename <> '_RETURN'", schema, prefix)
	policies := g.Table("pg_policy AS pol").Select("'policy', c.relname, pol.polname").
		Joins("JOIN pg_class AS c ON c.oid = pol.polrelid").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?)", schema, prefix)
	return g.Raw("(?) UNION ALL (?) UNION ALL (?) UNION ALL (?)", rels, triggers, rules, policies)
}

// dropCode drops the code objects of the plugin's prefix (codeObjects),
// in tx, which holds the plugin's locks and aclLock: the views first, then
// what is left of the triggers, rules and policies.
func (m *Manager) dropCode(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	var found []stray
	if err := codeObjects(g, m.schema, manifest.TablePrefix(pluginKey)).Scan(&found).Error; err != nil {
		return err
	}
	sid := store.Ident(m.schema)
	for _, s := range found {
		if err := dropStray(g, sid, s); err != nil {
			return err
		}
	}
	return nil
}

// grant is an entry of the privileges of an object of a plugin's prefix
// that its owner did not give itself: Kind is object.Kind, Grantee the
// role, "" for PUBLIC.
type grant struct {
	Kind    string `gorm:"column:kind"`
	Name    string `gorm:"column:name"`
	Grantee string `gorm:"column:grantee"`
}

// foreignGrants selects, as grant rows, the privileges on the relations
// (and their columns) and types of prefix in schema held by PUBLIC or a
// role other than the object's owner.
func foreignGrants(g *gorm.DB, schema, prefix string) *gorm.DB {
	const grantee = "CASE a.grantee WHEN 0 THEN '' ELSE pg_get_userbyid(a.grantee) END"
	rels := g.Table("pg_class AS c").Select("c.relkind::text AS kind, c.relname AS name, "+grantee+" AS grantee").
		Joins("CROSS JOIN LATERAL aclexplode(c.relacl) AS a").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?) AND a.grantee <> c.relowner", schema, prefix)
	cols := g.Table("pg_attribute AS t").Select("c.relkind::text, c.relname, "+grantee).
		Joins("JOIN pg_class AS c ON c.oid = t.attrelid").
		Joins("CROSS JOIN LATERAL aclexplode(t.attacl) AS a").
		Where("c.relnamespace = ?::regnamespace AND starts_with(c.relname, ?) AND t.attnum > 0 AND a.grantee <> c.relowner", schema, prefix)
	arrayOf := g.Table("pg_type AS e").Select("1").Where("e.typarray = t.oid")
	composite := g.Table("pg_class AS r").Select("1").Where("r.oid = t.typrelid AND r.relkind = 'c'")
	types := g.Table("pg_type AS t").Select("CASE t.typtype WHEN 'd' THEN 'domain' ELSE 'type' END, t.typname, "+grantee).
		Joins("CROSS JOIN LATERAL aclexplode(t.typacl) AS a").
		Where(`t.typnamespace = ?::regnamespace AND starts_with(t.typname, ?) AND t.typtype <> 'm' AND NOT EXISTS (?)
			AND (t.typrelid = 0 OR EXISTS (?)) AND a.grantee <> t.typowner`, schema, prefix, arrayOf, composite)
	return g.Raw("(?) UNION (?) UNION (?)", rels, cols, types)
}

// resetGrants takes back every privilege on the objects of the plugin's
// prefix that PUBLIC or a role other than the object's owner holds (a
// grant of the plugin, to PUBLIC or another plugin), in tx, which holds
// the plugin's locks. REVOKE ... CASCADE also takes what a grantee passed
// on; a grant that a pass leaves (one passed on by a role revoked later in
// the same pass) goes in the next.
func (m *Manager) resetGrants(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	sid := store.Ident(m.schema)
	for range 3 {
		var found []grant
		if err := foreignGrants(g, m.schema, manifest.TablePrefix(pluginKey)).Scan(&found).Error; err != nil {
			return err
		}
		if len(found) == 0 {
			return nil
		}
		if err := store.XactLock(ctx, tx, aclLock); err != nil {
			return err
		}
		for _, gr := range found {
			if err := revokeAll(g, gr.Kind, sid, store.Ident(gr.Name), gr.Grantee); err != nil {
				return fmt.Errorf("revoke the privileges on %s: %w", gr.Name, err)
			}
		}
	}
	return fmt.Errorf("privileges on objects named %s... held by other roles remain", manifest.TablePrefix(pluginKey))
}

// revokeAll takes back every privilege grantee ("" for PUBLIC) holds on
// the object schema.name of kind (object.Kind), and what it passed on.
func revokeAll(g *gorm.DB, kind string, schema, name store.Ident, grantee string) error {
	if grantee == "" {
		switch kind {
		case "S":
			return store.DDL(g, "REVOKE ALL ON SEQUENCE %s.%s FROM PUBLIC CASCADE", schema, name)
		case "type", "c":
			return store.DDL(g, "REVOKE ALL ON TYPE %s.%s FROM PUBLIC CASCADE", schema, name)
		case "domain":
			return store.DDL(g, "REVOKE ALL ON DOMAIN %s.%s FROM PUBLIC CASCADE", schema, name)
		default:
			return store.DDL(g, "REVOKE ALL ON TABLE %s.%s FROM PUBLIC CASCADE", schema, name)
		}
	}
	rid := store.Ident(grantee)
	switch kind {
	case "S":
		return store.DDL(g, "REVOKE ALL ON SEQUENCE %s.%s FROM %s CASCADE", schema, name, rid)
	case "type", "c":
		return store.DDL(g, "REVOKE ALL ON TYPE %s.%s FROM %s CASCADE", schema, name, rid)
	case "domain":
		return store.DDL(g, "REVOKE ALL ON DOMAIN %s.%s FROM %s CASCADE", schema, name, rid)
	default:
		return store.DDL(g, "REVOKE ALL ON TABLE %s.%s FROM %s CASCADE", schema, name, rid)
	}
}

// object is a relation or type of the plugin's prefix that has an owner of
// its own: Kind is the relkind of a relation, "type" or "domain".
type object struct {
	Kind string `gorm:"column:kind"`
	Name string `gorm:"column:name"`
}

// ownedObjects lists the objects of the plugin's prefix its identity owns
// (mine) or does not: the relations of prefixed (partitions included) and
// the types that are not part of another object.
func (m *Manager) ownedObjects(g *gorm.DB, pluginKey string, mine bool) ([]object, error) {
	role := m.Identity(pluginKey)
	rels := m.prefixed(g, pluginKey).Select("c.relkind::text AS kind, c.relname AS name")
	types := m.prefixedTypes(g, pluginKey).Select("CASE t.typtype WHEN 'd' THEN 'domain' ELSE 'type' END, t.typname")
	if mine {
		rels, types = rels.Where("pg_get_userbyid(c.relowner) = ?", role), types.Where("pg_get_userbyid(t.typowner) = ?", role)
	} else {
		rels, types = rels.Where("pg_get_userbyid(c.relowner) <> ?", role), types.Where("pg_get_userbyid(t.typowner) <> ?", role)
	}
	var out []object
	err := g.Raw("(?) UNION ALL (?)", rels, types).Scan(&out).Error
	return out, err
}

// prefixedTypes selects, as t, the types of the plugin's prefix in the
// core's schema that are not part of another object (a relation's row
// type, an array type, a range's multirange type).
func (m *Manager) prefixedTypes(g *gorm.DB, pluginKey string) *gorm.DB {
	arrayOf := g.Table("pg_type AS e").Select("1").Where("e.typarray = t.oid")
	return g.Table("pg_type AS t").
		Where("t.typnamespace = ?::regnamespace AND starts_with(t.typname, ?) AND t.typrelid = 0 AND t.typtype <> 'm' AND NOT EXISTS (?)",
			m.schema, manifest.TablePrefix(pluginKey), arrayOf)
}

// handBack gives the objects of the plugin's prefix the core or anyone
// else owns (a restore without owners, a reinstall after an uninstall that
// kept them, removeStrays) to its identity. The new owner must be able to
// create in the schema: CREATE for the rest of tx (ensureTx takes it back).
func (m *Manager) handBack(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	foreign, err := m.ownedObjects(g, pluginKey, false)
	if err != nil || len(foreign) == 0 {
		return err
	}
	role := m.Identity(pluginKey)
	if err := m.setSchemaCreate(ctx, tx, role, true); err != nil {
		return err
	}
	for _, o := range foreign {
		if err := alterOwner(g, o.Kind, store.Ident(m.schema), store.Ident(o.Name), role); err != nil {
			return fmt.Errorf("hand %s to role %s: %w", o.Name, role, err)
		}
	}
	return nil
}

// alterOwner makes owner ("CURRENT_USER" or a role) the owner of the
// object schema.name of kind (object.Kind).
func alterOwner(g *gorm.DB, kind string, schema, name store.Ident, owner string) error {
	if owner == "CURRENT_USER" {
		switch kind {
		case "c", "type":
			return store.DDL(g, "ALTER TYPE %s.%s OWNER TO CURRENT_USER", schema, name)
		case "domain":
			return store.DDL(g, "ALTER DOMAIN %s.%s OWNER TO CURRENT_USER", schema, name)
		default:
			return store.DDL(g, "ALTER TABLE %s.%s OWNER TO CURRENT_USER", schema, name)
		}
	}
	rid := store.Ident(owner)
	switch kind {
	case "c", "type":
		return store.DDL(g, "ALTER TYPE %s.%s OWNER TO %s", schema, name, rid)
	case "domain":
		return store.DDL(g, "ALTER DOMAIN %s.%s OWNER TO %s", schema, name, rid)
	default:
		return store.DDL(g, "ALTER TABLE %s.%s OWNER TO %s", schema, name, rid)
	}
}

// dropObject drops the object schema.name of kind (object.Kind) and what
// depends on it; one an earlier drop took with it is skipped.
func dropObject(g *gorm.DB, kind string, schema, name store.Ident) error {
	switch kind {
	case "type":
		return store.DDL(g, "DROP TYPE IF EXISTS %s.%s CASCADE", schema, name)
	case "domain":
		return store.DDL(g, "DROP DOMAIN IF EXISTS %s.%s CASCADE", schema, name)
	default:
		return dropRelation(g, kind, schema, name)
	}
}
