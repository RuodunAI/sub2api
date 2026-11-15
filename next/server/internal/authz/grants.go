package authz

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// Grants of a role (CONTRACTS §21): an unscoped key is a role_permissions
// row; a scoped one (account:read:group/12) a role_permission_scopes row of
// the action and the scope.

// grant is a resolved grant: the permission and its scope, "" for every row.
type grant struct {
	key   string // canonical
	id    int64
	scope string
}

// resolvePermissions canonicalizes keys, without duplicates or keys another
// implies (canonicalKeys), and resolves them to grants, dropping removed
// permissions. A key that does not parse, or names a plugin permission with
// a scope it does not take (pluginScope.fits), is invalid; one naming no
// permission is unknown.
func resolvePermissions(ctx context.Context, tx pgx.Tx, keys []string) ([]string, []grant, error) {
	var invalid, unknown []string
	pats := make([]perm.Pattern, 0, len(keys))
	actions := make([]string, 0, len(keys))
	for _, k := range dedupe(keys) {
		if _, err := perm.Parse(k); err != nil {
			invalid = append(invalid, k)
		}
	}
	if len(invalid) > 0 {
		return nil, nil, invalidKeys(invalid)
	}
	for _, k := range canonicalKeys(keys) {
		p, _ := perm.Parse(k)
		pats = append(pats, p)
		actions = append(actions, string(p.Action))
	}
	type row struct {
		id      int64
		removed bool
		scope   pluginScope
	}
	found := map[string]row{}
	rows, err := tx.Query(ctx, `SELECT key, id, status = 'removed', scope_type, scope FROM permissions WHERE key = ANY($1)`, actions)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var k, scopeType string
		var scope manifest.PermissionScope
		var r row
		if err := rows.Scan(&k, &r.id, &r.removed, &scopeType, &scope); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r.scope = readPluginScope(scopeType, &scope)
		found[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	kept := make([]string, 0, len(pats))
	grants := make([]grant, 0, len(pats))
	for _, p := range pats {
		r, ok := found[string(p.Action)]
		switch {
		case !ok:
			unknown = append(unknown, string(p.Key()))
		case isPluginAction(p.Action) && !r.scope.fits(p):
			invalid = append(invalid, string(p.Key()))
		case !r.removed:
			kept = append(kept, string(p.Key()))
			grants = append(grants, grant{key: string(p.Key()), id: r.id, scope: p.Suffix()})
		}
	}
	if len(invalid) > 0 {
		return nil, nil, invalidKeys(invalid)
	}
	if len(unknown) > 0 {
		return nil, nil, core.InvalidFields(core.FieldError{Field: "permission_keys", Code: "unknown", Message: "unknown permissions: " + strings.Join(unknown, ", ")})
	}
	return kept, grants, nil
}

// invalidKeys is the refusal of keys that do not parse or name a scope
// their permission does not take.
func invalidKeys(keys []string) error {
	return core.InvalidFields(core.FieldError{Field: "permission_keys", Code: "invalid", Message: "invalid permission keys: " + strings.Join(keys, ", ")})
}

// pluginScope is a plugin permission's scope as the catalog stores it
// (scope_type "plugin:<resource>", scope): the scopes a grant may name.
type pluginScope struct {
	resource string   // "" when the permission takes no scope
	own      bool     // "own" may be granted
	values   []string // enum keys; nil for an options or id resource
}

// readPluginScope reads a plugin permission's scope_type and scope (nil or
// zero for an unscoped one).
func readPluginScope(scopeType string, scope *manifest.PermissionScope) pluginScope {
	res, ok := strings.CutPrefix(scopeType, core.PluginScopeTypePrefix)
	if !ok {
		return pluginScope{}
	}
	ps := pluginScope{resource: res}
	if scope != nil {
		ps.own = scope.Own
		for _, v := range scope.Values {
			ps.values = append(ps.values, v.Key)
		}
	}
	return ps
}

// fits reports whether p's scope is one the permission takes: every row
// always; own when allowed; <resource>/<value> of its resource, and of its
// values when it lists them. A core-style scope never fits.
func (ps pluginScope) fits(p perm.Pattern) bool {
	switch s := p.Scope; {
	case s.All:
		return true
	case s.Own:
		return ps.own
	case s.Res == "" || s.Res != ps.resource:
		return false
	}
	return ps.values == nil || slices.Contains(ps.values, p.Scope.Value)
}

// isPluginAction reports whether a is a plugin permission's action, whose
// scopes the catalog checks (pluginScope) rather than perm.
func isPluginAction(a perm.Key) bool { return strings.HasPrefix(string(a), perm.PluginPrefix) }

// writeGrants makes grants the role's live grants, its core ones only when
// coreOnly (a reset leaves plugin permissions alone). Grants of removed
// permissions stay: they grant nothing, and revive with the key.
func writeGrants(ctx context.Context, tx pgx.Tx, roleID int64, grants []grant, coreOnly bool) error {
	// Never nil: a NULL array would make "<> ALL" delete nothing.
	ids, scopedIDs, scopes := []int64{}, []int64{}, []string{}
	for _, g := range grants {
		if g.scope == "" {
			ids = append(ids, g.id)
		} else {
			scopedIDs, scopes = append(scopedIDs, g.id), append(scopes, g.scope)
		}
	}
	source := ""
	if coreOnly {
		source = ` AND p.source = 'core'`
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions rp USING permissions p
WHERE rp.role_id = $1 AND p.id = rp.permission_id AND p.status <> 'removed'`+source+` AND rp.permission_id <> ALL($2)`, roleID, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, permission_id) SELECT $1, unnest($2::bigint[]) ON CONFLICT DO NOTHING`, roleID, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permission_scopes rs USING permissions p
WHERE rs.role_id = $1 AND p.id = rs.permission_id AND p.status <> 'removed'
  AND (rs.permission_id, rs.scope) NOT IN (SELECT * FROM unnest($2::bigint[], $3::text[]))`, roleID, scopedIDs, scopes); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO role_permission_scopes (role_id, permission_id, scope)
SELECT $1, * FROM unnest($2::bigint[], $3::text[]) ON CONFLICT DO NOTHING`, roleID, scopedIDs, scopes)
	return err
}

// roleGrantsJoin joins, to each role r, g.key: the canonical grants of r
// whose permission p passes status, an SQL condition on p.
func roleGrantsJoin(status string) string {
	return `
LEFT JOIN LATERAL (
    SELECT p.key FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id AND ` + status + ` WHERE rp.role_id = r.id
    UNION ALL
    SELECT p.key || ':' || rs.scope FROM role_permission_scopes rs JOIN permissions p ON p.id = rs.permission_id AND ` + status + `
    WHERE rs.role_id = r.id
) g ON true`
}

// canonicalKeys is keys canonical, in order, without duplicates or keys
// another of them implies (account:read:own next to account:read): what a
// role editor shows and saves back.
func canonicalKeys(keys []string) []string {
	set := core.PermissionSetOf(keys...)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		c, err := perm.Canonical(k)
		if err != nil || slices.Contains(out, string(c)) || impliedByOther(set, c) {
			continue
		}
		out = append(out, string(c))
	}
	return out
}

// impliedByOther reports whether set holds a key other than k implying k.
func impliedByOther(set core.PermissionSet, k perm.Key) bool {
	p, _ := perm.Parse(string(k))
	if p.Scope.All {
		return false
	}
	if _, ok := set.Keys[string(p.Action)]; ok {
		return true
	}
	for _, sc := range set.Scoped[p.Action] {
		if sc != p.Scope && perm.Implies(perm.Pattern{Action: p.Action, Scope: sc}, p) {
			return true
		}
	}
	return false
}

// notImplied returns the keys of want that have does not imply, in order:
// what a write adds beyond what a role already grants.
func notImplied(want, have []string) []string {
	set := core.PermissionSetOf(have...)
	return slices.DeleteFunc(slices.Clone(want), set.Has)
}
