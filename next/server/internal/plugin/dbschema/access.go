package dbschema

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/grants"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// AccessPermission is the host permission whose grant carries a plugin's
// access to tables outside it (manifest database.access).
const AccessPermission = "db.core_read"

// AccessScope is the scope of AccessPermission for access: {"access":
// ["<privilege>:<table>.<column>", ...]}, "<table>.*" for a whole table
// (no columns, and always for delete, which has no column form). The grant
// stores it, so an upgrade that asks for anything more - a table, a
// column, a privilege - is a wider scope and is consented to again, and
// the grants the identity gets (syncAccess) are the granted scope's.
func AccessScope(access []manifest.DatabaseAccess) map[string]any {
	list := []any{}
	for _, a := range access {
		for _, p := range a.Privileges {
			if len(a.Columns) == 0 || p == "delete" {
				list = append(list, p+":"+a.Table+".*")
				continue
			}
			for _, c := range a.Columns {
				list = append(list, p+":"+a.Table+"."+c)
			}
		}
	}
	return map[string]any{"access": list}
}

// accessEntryRe is an entry of AccessScope.
var accessEntryRe = regexp.MustCompile(`^(select|insert|update|delete|references|usage):([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*|\*)$`)

// grantedAccess is the set of AccessScope entries the plugin's
// AccessPermission grant holds, for the version the plugin runs or moves
// to (an upgrade migrates under the new version's grants), plus USAGE on
// the sequences of the columns of each table it may insert into, which
// nextval needs.
func (m *Manager) grantedAccess(ctx context.Context, tx *gorm.DB, pluginKey string) (map[string]bool, error) {
	g := store.G(ctx, tx)
	want := map[string]bool{}
	var versions []string
	if err := g.Table("plugins").Where("key = ? AND COALESCE(desired_version, active_version) IS NOT NULL", pluginKey).
		Pluck("COALESCE(desired_version, active_version)", &versions).Error; err != nil || len(versions) == 0 {
		return want, err
	}
	granted, err := grants.Granted(ctx, tx, pluginKey, versions[0], []manifest.HostPermission{{ID: AccessPermission}})
	if err != nil || granted[AccessPermission] == nil {
		return want, err
	}
	var scope struct{ Access []string }
	if err := json.Unmarshal(granted[AccessPermission], &scope); err != nil {
		return nil, fmt.Errorf("scope of %s: %w", AccessPermission, err)
	}
	var inserted []string
	for _, e := range scope.Access {
		sub := accessEntryRe.FindStringSubmatch(e)
		if sub == nil || sub[1] == "usage" || sub[1] == "delete" && sub[3] != "*" {
			return nil, fmt.Errorf("scope of %s: invalid entry %q", AccessPermission, e)
		}
		want[e] = true
		if sub[1] == "insert" {
			inserted = append(inserted, sub[2])
		}
	}
	if len(inserted) > 0 {
		var seqs []string
		if err := columnSequences(g, m.schema, inserted).Pluck("s.relname", &seqs).Error; err != nil {
			return nil, err
		}
		for _, s := range seqs {
			want["usage:"+s+".*"] = true
		}
	}
	return want, nil
}

// columnSequences selects, as s, the sequences of the columns (serial) of
// tables in schema.
func columnSequences(g *gorm.DB, schema string, tables []string) *gorm.DB {
	return g.Table("pg_depend AS d").
		Joins("JOIN pg_class AS s ON s.oid = d.objid AND s.relkind = 'S'").
		Joins("JOIN pg_class AS t ON t.oid = d.refobjid").
		Where("d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a'").
		Where("t.relnamespace = ?::regnamespace AND t.relname = ANY(?)", schema, store.Strings(tables))
}

// heldAccess is the set of AccessScope entries the identity holds now on
// relations of the core's schema outside the plugin's prefix, tables and
// sequences ("usage:<sequence>.*").
func (m *Manager) heldAccess(g *gorm.DB, pluginKey string) (map[string]bool, error) {
	role := m.Identity(pluginKey)
	prefix := manifest.TablePrefix(pluginKey)
	tables := g.Table("pg_class AS c").
		Select("lower(a.privilege_type) || ':' || c.relname || '.*' AS entry").
		Joins("CROSS JOIN LATERAL aclexplode(c.relacl) AS a").
		Where("c.relnamespace = ?::regnamespace AND NOT starts_with(c.relname, ?) AND a.grantee = ?::regrole", m.schema, prefix, role)
	columns := g.Table("pg_class AS c").
		Select("lower(a.privilege_type) || ':' || c.relname || '.' || att.attname").
		Joins("JOIN pg_attribute AS att ON att.attrelid = c.oid").
		Joins("CROSS JOIN LATERAL aclexplode(att.attacl) AS a").
		Where("c.relnamespace = ?::regnamespace AND NOT starts_with(c.relname, ?) AND a.grantee = ?::regrole", m.schema, prefix, role)
	var entries []string
	if err := g.Raw("(?) UNION ALL (?)", tables, columns).Pluck("entry", &entries).Error; err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(entries))
	for _, e := range entries {
		held[e] = true
	}
	return held, nil
}

// syncAccess makes the identity's grants outside the plugin's prefix what
// its AccessPermission grant holds (grantedAccess): when they differ it
// revokes every one and grants them again, column by column as asked. It
// also grants USAGE on the core's schema when PUBLIC does not have it.
func (m *Manager) syncAccess(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	g := store.G(ctx, tx)
	role := m.Identity(pluginKey)
	rid, sid := store.Ident(role), store.Ident(m.schema)
	want, err := m.grantedAccess(ctx, tx, pluginKey)
	if err != nil {
		return err
	}
	held, err := m.heldAccess(g, pluginKey)
	if err != nil {
		return err
	}
	var usage bool
	if err := g.Table("pg_roles").Select("has_schema_privilege(oid, ?::regnamespace, 'USAGE')", m.schema).Where("rolname = ?", role).Row().Scan(&usage); err != nil {
		return err
	}
	if usage && maps.Equal(want, held) {
		return nil
	}
	if err := store.XactLock(ctx, tx, aclLock); err != nil {
		return err
	}
	if !usage {
		if err := store.DDL(g, "GRANT USAGE ON SCHEMA %s TO %s", sid, rid); err != nil {
			return err
		}
	}
	revoked := map[string]bool{}
	for e := range held {
		_, rel, _ := strings.Cut(e, ":")
		table, _, _ := strings.Cut(rel, ".")
		if revoked[table] {
			continue
		}
		revoked[table] = true
		if err := store.DDL(g, "REVOKE ALL ON TABLE %s.%s FROM %s", sid, store.Ident(table), rid); err != nil {
			return err
		}
	}
	// One GRANT per table and privilege, on the columns asked for or the
	// whole table.
	type target struct{ privilege, table string }
	columns := map[target][]string{}
	for e := range want {
		sub := accessEntryRe.FindStringSubmatch(e)
		t := target{sub[1], sub[2]}
		if sub[3] == "*" {
			columns[t] = nil
		} else if c, ok := columns[t]; !ok || c != nil {
			columns[t] = append(c, sub[3])
		}
	}
	for _, t := range slices.SortedFunc(maps.Keys(columns), func(a, b target) int {
		return strings.Compare(a.table+":"+a.privilege, b.table+":"+b.privilege)
	}) {
		var err error
		if t.privilege == "usage" {
			err = store.DDL(g, "GRANT USAGE ON SEQUENCE %s.%s TO %s", sid, store.Ident(t.table), rid)
		} else {
			cols := columns[t]
			slices.Sort(cols)
			err = store.DDL(g, "GRANT %s ON TABLE %s.%s TO %s", store.Privileges{Names: []string{t.privilege}, Columns: cols}, sid, store.Ident(t.table), rid)
		}
		if err != nil {
			return fmt.Errorf("grant %s on %s to role %s: %w", t.privilege, t.table, role, err)
		}
	}
	return nil
}

// SyncAccessTx sets the identity's grants outside the plugin's prefix to
// its AccessPermission grant, in tx: the consent, an administrator's change
// or revocation of that grant takes effect in the database when tx
// commits. Nothing happens before the plugin's identity exists (its first
// Ensure grants them).
func (m *Manager) SyncAccessTx(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	if err := validKey(pluginKey); err != nil {
		return err
	}
	if err := lockPlugin(ctx, tx, pluginKey); err != nil {
		return err
	}
	// This node's next Ensure goes through again rather than trust a state
	// that tx may change.
	m.mu.Lock()
	delete(m.ensured, pluginKey)
	m.mu.Unlock()
	g := store.G(ctx, tx)
	exists, err := m.identityExists(g, pluginKey)
	if err != nil || !exists {
		return err
	}
	return m.syncAccess(ctx, tx, pluginKey)
}

// CheckAccess reports the tables and columns of access that are not in
// the core's schema, as field errors of database.access.
func (m *Manager) CheckAccess(ctx context.Context, access []manifest.DatabaseAccess) ([]core.FieldError, error) {
	g := store.G(ctx, m.db.Gorm)
	var errs []core.FieldError
	for i, a := range access {
		var cols []string
		err := g.Table("pg_attribute AS att").
			Joins("JOIN pg_class AS c ON c.oid = att.attrelid").
			Where("c.relnamespace = ?::regnamespace AND c.relname = ? AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND att.attnum > 0 AND NOT att.attisdropped", m.schema, a.Table).
			Pluck("att.attname", &cols).Error
		if err != nil {
			return nil, err
		}
		f := fmt.Sprintf("database.access[%d]", i)
		if len(cols) == 0 {
			errs = append(errs, core.FieldError{Field: f + ".table", Code: "unknown", Message: fmt.Sprintf("the core has no table %q", a.Table)})
			continue
		}
		for j, c := range a.Columns {
			if !slices.Contains(cols, c) {
				errs = append(errs, core.FieldError{Field: fmt.Sprintf("%s.columns[%d]", f, j), Code: "unknown", Message: fmt.Sprintf("table %q has no column %q", a.Table, c)})
			}
		}
	}
	return errs, nil
}
