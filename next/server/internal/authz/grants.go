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
	key      string // canonical
	id       int64
	scope    string
	critical bool // of a critical plugin permission (stepUpCritical)
}

// resolvePermissions canonicalizes keys, without duplicates or keys another
// implies (canonicalKeys), and resolves them to grants: live lists the keys
// of permissions not removed, grants all of them, so that a write keeps the
// removed ones listed (writeGrants never adds one). A key that does not
// parse, or names a plugin permission with a scope it does not take
// (manifest.PermissionScope.Takes), is invalid; one naming no permission
// is unknown.
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
		risk    string
		scope   manifest.PermissionScope
	}
	found := map[string]row{}
	rows, err := tx.Query(ctx, `SELECT key, id, status = 'removed', risk, scope_type, scope FROM permissions WHERE key = ANY($1)`, actions)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var k, scopeType string
		var scope manifest.PermissionScope
		var r row
		if err := rows.Scan(&k, &r.id, &r.removed, &r.risk, &scopeType, &scope); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r.scope = core.ReadPluginScope(scopeType, &scope)
		found[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	live := make([]string, 0, len(pats))
	grants := make([]grant, 0, len(pats))
	for _, p := range pats {
		r, ok := found[string(p.Action)]
		switch {
		case !ok:
			unknown = append(unknown, string(p.Key()))
		case isPluginAction(p.Action) && !r.scope.Takes(p.Suffix()):
			invalid = append(invalid, string(p.Key()))
		default:
			if !r.removed {
				live = append(live, string(p.Key()))
			}
			critical := isPluginAction(p.Action) && r.risk == manifest.RiskCritical
			grants = append(grants, grant{key: string(p.Key()), id: r.id, scope: p.Suffix(), critical: critical})
		}
	}
	if len(invalid) > 0 {
		return nil, nil, invalidKeys(invalid)
	}
	if len(unknown) > 0 {
		return nil, nil, core.InvalidFields(core.FieldError{Field: "permission_keys", Code: "unknown", Message: "unknown permissions: " + strings.Join(unknown, ", ")})
	}
	return live, grants, nil
}

// invalidKeys is the refusal of keys that do not parse or name a scope
// their permission does not take.
func invalidKeys(keys []string) error {
	return core.InvalidFields(core.FieldError{Field: "permission_keys", Code: "invalid", Message: "invalid permission keys: " + strings.Join(keys, ", ")})
}

// isPluginAction reports whether a is a plugin permission's action, whose
// scopes the catalog checks (core.ReadPluginScope) rather than perm.
func isPluginAction(a perm.Key) bool { return strings.HasPrefix(string(a), perm.PluginPrefix) }

// grantPart is the part of a role's grants a write replaces: all of them,
// the core ones (a reset), or one plugin's.
type grantPart struct {
	coreOnly bool
	plugin   string // a plugin key; "" for none
}

// of is the keys of keys within part, in order.
func (part grantPart) of(keys []string) []string {
	prefix := manifest.PermissionModule(part.plugin) + ":"
	out := []string{}
	for _, k := range keys {
		switch {
		case part.plugin != "" && !strings.HasPrefix(k, prefix):
		case part.coreOnly && strings.HasPrefix(k, perm.PluginPrefix):
		default:
			out = append(out, k)
		}
	}
	return out
}

// grantsInPart is the SQL condition on p, a granted permission, of the
// grants a write of part $2 (coreOnly) and $3 (plugin) replaces. A reset
// keeps the grants of removed permissions, to come back with the key; any
// other write drops them unless listed, so an editor can untick them.
const grantsInPart = `(NOT $2::bool OR (p.source = 'core' AND p.status <> 'removed')) AND ($3::text = '' OR p.plugin_key = $3::text)`

// writeGrants makes grants the role's grants within part. It never adds a
// grant of a removed permission: listing one keeps it when the role holds
// it, and is ignored when not (it can only be unticked).
func writeGrants(ctx context.Context, tx pgx.Tx, roleID int64, grants []grant, part grantPart) error {
	// Never nil: a NULL array would make "<> ALL" delete nothing.
	ids, scopedIDs, scopes := []int64{}, []int64{}, []string{}
	for _, g := range grants {
		if g.scope == "" {
			ids = append(ids, g.id)
		} else {
			scopedIDs, scopes = append(scopedIDs, g.id), append(scopes, g.scope)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions rp USING permissions p
WHERE rp.role_id = $1 AND p.id = rp.permission_id AND `+grantsInPart+` AND rp.permission_id <> ALL($4)`,
		roleID, part.coreOnly, part.plugin, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, permission_id)
SELECT $1, id FROM permissions WHERE id = ANY($2) AND status <> 'removed' ON CONFLICT DO NOTHING`, roleID, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permission_scopes rs USING permissions p
WHERE rs.role_id = $1 AND p.id = rs.permission_id AND `+grantsInPart+`
  AND (rs.permission_id, rs.scope) NOT IN (SELECT * FROM unnest($4::bigint[], $5::text[]))`,
		roleID, part.coreOnly, part.plugin, scopedIDs, scopes); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO role_permission_scopes (role_id, permission_id, scope)
SELECT $1, g.id, g.scope FROM unnest($2::bigint[], $3::text[]) g (id, scope)
JOIN permissions p ON p.id = g.id AND p.status <> 'removed' ON CONFLICT DO NOTHING`, roleID, scopedIDs, scopes)
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

// withSuffix is the key of the permission key at scope suffix sfx, key
// itself for "" (every row).
func withSuffix(key, sfx string) string {
	if sfx == "" {
		return key
	}
	return key + ":" + sfx
}

// notImplied returns the keys of want that have does not imply, in order:
// what a write adds beyond what a role already grants.
func notImplied(want, have []string) []string {
	set := core.PermissionSetOf(have...)
	return slices.DeleteFunc(slices.Clone(want), set.Has)
}
