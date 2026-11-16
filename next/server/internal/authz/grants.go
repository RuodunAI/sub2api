package authz

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Grants of a role (CONTRACTS §21): a p row of casbin_rule per key, an
// unscoped one (account:read) of scope every row, a scoped one
// (account:read:group/12) of the action and the scope (casbinrule).

// grant is a resolved grant: the permission and its scope, "" for every row.
type grant struct {
	key      string // canonical
	action   string // the permission's key
	scope    string
	removed  bool // of a removed permission: kept when held, never added
	critical bool // of a critical plugin permission (stepUpCritical)
}

// resolvePermissions canonicalizes keys, without duplicates or keys another
// implies (canonicalKeys), and resolves them to grants: live lists the keys
// of permissions not removed, grants all of them, so that a write keeps the
// removed ones listed (writeGrants never adds one). A key that does not
// parse, or names a plugin permission with a scope it does not take
// (manifest.PermissionScope.Takes), is invalid; one naming no permission
// is unknown.
func resolvePermissions(ctx context.Context, tx *gorm.DB, keys []string) ([]string, []grant, error) {
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
		removed bool
		risk    string
		scope   manifest.PermissionScope
	}
	found := map[string]row{}
	// A staged permission (core.PermissionDef.Staged) is unknown here: it
	// belongs to a version not activated yet, whose consent alone grants it.
	if err := store.EachRow(grantablePermissions(store.G(ctx, tx), actions), func(rows *sql.Rows) error {
		var k, scopeType string
		var scope store.JSON[manifest.PermissionScope]
		var r row
		if err := rows.Scan(&k, &r.removed, &r.risk, &scopeType, &scope); err != nil {
			return err
		}
		r.scope = core.ReadPluginScope(scopeType, &scope.V)
		found[k] = r
		return nil
	}); err != nil {
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
			grants = append(grants, grant{key: string(p.Key()), action: string(p.Action), scope: p.Suffix(), removed: r.removed, critical: critical})
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

// grantablePermissions selects the key, whether removed, risk, scope type
// and scope of the permissions keys names, staged ones aside.
func grantablePermissions(db *gorm.DB, keys []string) *gorm.DB {
	return db.Raw(`SELECT key, status = 'removed', risk, scope_type, scope FROM permissions
WHERE key = ANY(?) AND status <> 'staged'`, store.Strings(keys))
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
		if (part.plugin != "" && !strings.HasPrefix(k, prefix)) || (part.coreOnly && strings.HasPrefix(k, perm.PluginPrefix)) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// filter selects the grants a write of part replaces. A reset keeps the
// grants of removed permissions, to come back with the key; any other
// write drops them unless listed, so an editor can untick them. No write
// touches the grants of a staged permission, which no editor sees: its
// version's consent made them.
func (part grantPart) filter() casbinrule.Filter {
	f := casbinrule.Filter{Plugin: part.plugin, Skip: []string{"staged"}}
	if part.coreOnly {
		f.Source, f.Skip = "core", append(f.Skip, "removed")
	}
	return f
}

// writeGrants makes grants the grants of the role key within part. It never
// adds a grant of a removed permission: listing one keeps it when the role
// holds it, and is ignored when not (it can only be unticked). Nor of a
// staged one (resolvePermissions refuses it as unknown first, in the same
// transaction, after Bump: no status changes in between).
func writeGrants(ctx context.Context, tx *gorm.DB, key string, grants []grant, part grantPart) error {
	keep, add := make([]casbinrule.Grant, 0, len(grants)), make([]casbinrule.Grant, 0, len(grants))
	for _, g := range grants {
		rg := casbinrule.Grant{Role: key, Action: g.action, Suffix: g.scope}
		keep = append(keep, rg)
		if !g.removed {
			add = append(add, rg)
		}
	}
	db := store.G(ctx, tx)
	if err := casbinrule.DropGrants(db, key, part.filter(), keep); err != nil {
		return err
	}
	_, err := casbinrule.AddGrants(db, add, casbinrule.Filter{})
	return err
}

// roleGrantsJoin joins, to each role r, g.key: the canonical grants of r
// whose permission p passes status, an SQL condition on p.
func roleGrantsJoin(status string) string {
	return `
LEFT JOIN LATERAL (
    SELECT rg.key FROM (` + casbinrule.GrantsOf("r.key") + `) rg JOIN permissions p ON p.key = rg.action AND ` + status + `
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

// scopeRefPatterns match the scopes naming a row of kind (CONTRACTS §21.0):
// account/X and group/G/account/X; group/G and group/G/account/X; proxy/X.
var scopeRefPatterns = map[string]string{
	core.ScopeRefAccount: `(^|/)account/%d$`,
	core.ScopeRefGroup:   `^group/%d(/|$)`,
	core.ScopeRefProxy:   `^proxy/%d$`,
}

// scopeRefs returns the regular expressions matching the scopes that name
// one of the rows ids of kind.
func scopeRefs(kind string, ids []int64) ([]string, error) {
	pattern, ok := scopeRefPatterns[kind]
	if !ok {
		return nil, fmt.Errorf("authz: unknown scope reference kind %q", kind)
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf(pattern, id)
	}
	return out, nil
}

// ClearScopeGrants implements core.ScopeGrantClearer: it deletes, inside
// tx, the scoped grants that name the deleted rows ids of kind
// (core.ScopeRefAccount, ...), audited as authz.scope.clear by actorID (0 =
// the system) per role, and moves both versions and those roles'
// revisions when any went. A grant naming a deleted row reaches nothing
// anyway; clearing it keeps roles readable and a reused id from being
// reached. It returns the new authz version, 0 when nothing went, and
// announces it itself once tx has committed (committedLater), as the
// plugin catalog writes do.
//
// It takes the authz lock first, as every authz write does, and before it
// reads: a role save that adds a grant naming one of ids holds the lock
// until it commits, so the delete below, a statement started after the
// lock, sees that grant (no grant naming a row deleted by tx survives).
// Grants name existing rows only (checkScopeRefs), so none can name a
// deleted one afterwards either.
func (s *Service) ClearScopeGrants(ctx context.Context, tx *gorm.DB, actorID int64, kind string, ids []int64) (int64, error) {
	patterns, err := scopeRefs(kind, ids)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	var actor *int64
	if actorID > 0 {
		actor = &actorID
	}
	db := store.G(ctx, tx)
	if err := lockVersion(db).Error; err != nil {
		return 0, err
	}
	// No row: nothing named ids; the versions stay.
	var c authzChange
	if err := clearScopeGrants(db, patterns, actor, kind, ids).Row().Scan(&c.Version, &c.Policy); err != nil {
		if store.IsNoRows(err) {
			return 0, nil
		}
		return 0, err
	}
	s.mu.Lock()
	s.pending[c.Version] = c
	s.mu.Unlock()
	s.committedLater(c.Version)
	return c.Version, nil
}

// clearScopeGrants deletes the scoped grants matching patterns, audits them
// per role and moves those roles' revisions and both versions, returning
// the versions when any went.
func clearScopeGrants(db *gorm.DB, patterns []string, actor *int64, kind string, ids []int64) *gorm.DB {
	return db.Raw(`WITH cleared AS (
    `+casbinrule.DeleteScopedSQL+`
), audited AS (
    INSERT INTO audit_logs (user_id, action, target_type, target_id, detail)
    SELECT ?, 'authz.scope.clear', 'role', role_id::text,
        jsonb_build_object('kind', ?::text, 'ids', to_jsonb(?::bigint[]), 'keys', to_jsonb(array_agg(key ORDER BY key)))
    FROM cleared GROUP BY role_id
), revised AS (
    `+casbinrule.ReviseSQL("cleared")+`
)
UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1 WHERE id = 1 AND EXISTS (SELECT 1 FROM cleared)
RETURNING version, policy_version`, store.Strings(patterns), actor, kind, store.Int64s(ids))
}

// scopeRefTables are the tables of the rows a scope names, by kind, with
// the condition of a live row: what checkScopeRefs looks the ids up in.
var scopeRefTables = map[string]string{
	core.ScopeRefAccount: `accounts WHERE deleted_at IS NULL AND`,
	core.ScopeRefGroup:   `groups WHERE`,
	core.ScopeRefProxy:   `proxies WHERE`,
}

// scopeRefsOf returns the rows the scope of p names, by kind: its group
// (group/G) and its account or proxy (account/X, group/G/account/X,
// proxy/X).
func scopeRefsOf(p perm.Pattern) map[string]int64 {
	out := map[string]int64{}
	if p.Scope.Group > 0 {
		out[core.ScopeRefGroup] = p.Scope.Group
	}
	if p.Scope.ID > 0 {
		out[string(perm.ScopeType(p.Action))] = p.Scope.ID
	}
	return out
}

// checkScopeRefs refuses keys whose scope names a row that does not exist
// (a deleted account, group or proxy, or one never created): such a grant
// would reach nothing now and the row of a reused id later, and the
// clearing of a delete (ClearScopeGrants) would never see it. Only the
// keys a write adds are checked: a grant a role holds already stays.
func checkScopeRefs(ctx context.Context, tx *gorm.DB, keys []string) error {
	var kinds, refKeys []string
	var ids []int64
	for _, k := range keys {
		p, err := perm.Parse(k)
		if err != nil {
			continue
		}
		for kind, id := range scopeRefsOf(p) {
			kinds, ids, refKeys = append(kinds, kind), append(ids, id), append(refKeys, k)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var missing []string
	if err := missingScopeRefs(store.G(ctx, tx), refKeys, kinds, ids).Scan(&missing).Error; err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	return core.InvalidFields(core.FieldError{Field: "permission_keys", Code: "unknown_scope",
		Message: "the scope names a row that does not exist: " + strings.Join(dedupe(missing), ", ")})
}

// missingScopeRefs selects the keys of keys whose row (kinds[i], ids[i])
// does not exist.
func missingScopeRefs(db *gorm.DB, keys, kinds []string, ids []int64) *gorm.DB {
	exists := make([]string, 0, len(scopeRefTables))
	for _, kind := range slices.Sorted(maps.Keys(scopeRefTables)) {
		exists = append(exists, `WHEN '`+kind+`' THEN EXISTS (SELECT 1 FROM `+scopeRefTables[kind]+` id = t.id)`)
	}
	return db.Raw(`SELECT t.key FROM unnest(?::text[], ?::text[], ?::bigint[]) AS t(key, kind, id)
WHERE NOT CASE t.kind `+strings.Join(exists, " ")+` ELSE false END
ORDER BY 1`, store.Strings(keys), store.Strings(kinds), store.Int64s(ids))
}
