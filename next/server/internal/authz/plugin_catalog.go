package authz

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// runningPluginStatuses are the plugins.status values (install.StatusEnabled,
// install.StatusUpgrading) under which a plugin serves traffic; a test keeps
// the list in step with the install package.
const runningPluginStatuses = `'enabled','upgrading'`

// SyncPlugin implements core.PermissionCatalog. A new permission follows the
// plugin: active while the plugin runs, disabled otherwise (SetPluginActive
// turns it on at enable), and is granted to its GrantRoles and to the
// holders of its InheritFrom keys (grantInherited), each grant audited as
// authz.sync.grant. Existing ones keep their status and role grants but the
// scoped ones their new scope no longer takes (pruneScopes); a removed one
// declared again revives with the plugin's status and the grants it had,
// never its GrantRoles (D6). Keys no longer declared are marked removed
// last: their grants stay, granting nothing, until the key comes back.
func (s *Service) SyncPlugin(ctx context.Context, tx pgx.Tx, pluginKey string, defs []core.PermissionDef) error {
	if pluginKey == "" {
		return core.ErrInvalidArgument.WithMessage("plugin key is required")
	}
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(defs))
	var created []core.PermissionDef
	for _, d := range defs {
		key := manifest.PermissionKey(pluginKey, d.Key)
		local, ok := manifest.LocalPermission(pluginKey, key)
		if !ok {
			return core.ErrInvalidArgument.WithMessage("empty plugin permission key")
		}
		d.Key, d.Risk = key, cmp.Or(d.Risk, manifest.RiskLow)
		keys = append(keys, key)
		if err := checkPluginDef(d); err != nil {
			return err
		}
		inserted, err := upsertPluginPermission(ctx, tx, pluginKey, local, d)
		if err != nil {
			return err
		}
		if inserted {
			created = append(created, d)
		} else if err := s.pruneScopes(ctx, tx, key, readPluginScope(d.ScopeType, d.Scope)); err != nil {
			return err
		}
	}
	for _, d := range created {
		if err := s.grantPlugin(ctx, tx, "default", d.Key, d.GrantRoles); err != nil {
			return err
		}
	}
	// Only grants that exist before this sync are inherited: a key created
	// now has none yet but the defaults just written, which are not its
	// holders' rights, so it is left out of every from list.
	isNew := make(map[string]bool, len(created))
	for _, d := range created {
		isNew[d.Key] = true
	}
	for _, d := range created {
		from := make([]string, 0, len(d.InheritFrom))
		for _, k := range d.InheritFrom {
			if key := manifest.PermissionKey(pluginKey, k); !isNew[key] {
				from = append(from, key)
			}
		}
		if from = dedupe(from); len(from) > 0 {
			if err := s.grantInherited(ctx, tx, d.Key, from); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE permissions SET status = 'removed'
WHERE plugin_key = $1 AND status <> 'removed' AND key <> ALL($2)`, pluginKey, keys); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// checkPluginDef fails (400) on a plugin permission definition the catalog
// would store wrong: a risk out of the three levels, a critical one with
// defaults, or GrantRoles and Defaults naming scopes it does not take. The
// manifest check catches all of these first; this keeps the catalog right
// whatever the caller.
func checkPluginDef(d core.PermissionDef) error {
	switch {
	case !slices.Contains(manifest.UserPermissionRisks, d.Risk):
		return core.ErrInvalidArgument.WithMessage("permission " + d.Key + ": unknown risk " + d.Risk)
	case d.Risk == manifest.RiskCritical && len(d.Defaults) > 0:
		return core.ErrInvalidArgument.WithMessage("permission " + d.Key + ": a critical permission has no defaults")
	}
	ps := readPluginScope(d.ScopeType, d.Scope)
	if err := checkScopes(d.Key, ps, d.Defaults); err != nil {
		return err
	}
	return checkScopes(d.Key, ps, d.GrantRoles)
}

// checkScopes fails (400 invalid) unless every suffix of roles is a scope
// of key that ps takes ("" every row always is).
func checkScopes(key string, ps pluginScope, roles map[string][]string) error {
	var invalid []string
	for _, suffixes := range roles {
		for _, sfx := range suffixes {
			if sfx == "" {
				continue
			}
			k := key + ":" + sfx
			if p, err := perm.Parse(k); err != nil || string(p.Action) != key || !ps.fits(p) {
				invalid = append(invalid, k)
			}
		}
	}
	if len(invalid) > 0 {
		slices.Sort(invalid)
		return invalidKeys(slices.Compact(invalid))
	}
	return nil
}

// upsertPluginPermission writes d, a plugin permission (d.Key qualified,
// local its plugin-local part), reporting whether it was created. sensitive
// is still written (risk <> 'low') for the previous core, should it run
// again; this one never reads it. A key of another owner is a conflict.
func upsertPluginPermission(ctx context.Context, tx pgx.Tx, pluginKey, local string, d core.PermissionDef) (bool, error) {
	label := d.Label
	if label.Get("en") == "" {
		label = core.LocalizedText{"en": local}
	}
	var scope manifest.PermissionScope
	if d.Scope != nil {
		scope = *d.Scope
	}
	var inserted bool
	err := tx.QueryRow(ctx, `
INSERT INTO permissions (key, module, label, description, source, plugin_key, risk, sensitive, status, sort,
    scope_type, scope, group_key, group_label, group_sort, requires, defaults)
VALUES ($1, $2, $3, $4, 'plugin', $5::text, $6::varchar, $6::varchar <> 'low',
    CASE WHEN EXISTS (SELECT 1 FROM plugins WHERE key = $5::text AND status IN (`+runningPluginStatuses+`))
         THEN 'active' ELSE 'disabled' END,
    $7, $8, $9, $10, $11, $12, COALESCE($13::text[], '{}'), $14)
ON CONFLICT (key) DO UPDATE SET module = EXCLUDED.module, label = EXCLUDED.label,
    description = EXCLUDED.description, risk = EXCLUDED.risk, sensitive = EXCLUDED.sensitive,
    sort = EXCLUDED.sort, scope_type = EXCLUDED.scope_type, scope = EXCLUDED.scope,
    group_key = EXCLUDED.group_key, group_label = EXCLUDED.group_label, group_sort = EXCLUDED.group_sort,
    requires = EXCLUDED.requires, defaults = EXCLUDED.defaults,
    -- a removed key declared again revives with the plugin's status; others keep theirs
    status = CASE WHEN permissions.status = 'removed' THEN EXCLUDED.status ELSE permissions.status END
    WHERE permissions.plugin_key = EXCLUDED.plugin_key
RETURNING (xmax = 0)`, d.Key, manifest.PermissionModule(pluginKey), label, orEmpty(d.Description), pluginKey,
		d.Risk, d.Sort, d.ScopeType, scope, d.Group, orEmpty(d.GroupLabel), d.GroupSort,
		d.Requires, orEmpty(d.Defaults)).Scan(&inserted)
	if store.IsNoRows(err) {
		return false, core.ErrConflict.WithMessage("permission " + d.Key + " belongs to another owner")
	}
	return inserted, err
}

// orEmpty is m, or an empty map for nil: a JSON object, never null.
func orEmpty[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

// pruneScopes deletes the scope grants of the plugin permission key that
// ps, its scope as now declared, no longer takes, each audited as
// authz.sync.revoke (via scope_changed). They must go rather than merely
// stop implying: an own or enum grant would still match at run time. Grants
// of every row always stay.
func (s *Service) pruneScopes(ctx context.Context, tx pgx.Tx, key string, ps pluginScope) error {
	var scopes []string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT rs.scope), '{}') FROM role_permission_scopes rs
JOIN permissions p ON p.id = rs.permission_id WHERE p.key = $1`, key).Scan(&scopes); err != nil {
		return err
	}
	stale := slices.DeleteFunc(scopes, func(sc string) bool {
		p, err := perm.Parse(key + ":" + sc)
		return err == nil && ps.fits(p)
	})
	if len(stale) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `DELETE FROM role_permission_scopes rs USING permissions p
WHERE p.id = rs.permission_id AND p.key = $1 AND rs.scope = ANY($2) RETURNING rs.role_id, rs.scope`, key, stale)
	if err != nil {
		return err
	}
	type revoked struct {
		role  int64
		scope string
	}
	gone, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (revoked, error) {
		var g revoked
		return g, r.Scan(&g.role, &g.scope)
	})
	if err != nil {
		return err
	}
	for _, g := range gone {
		if err := s.audit(ctx, tx, 0, "authz.sync.revoke", "role", g.role, map[string]any{"via": "scope_changed", "key": key + ":" + g.scope}); err != nil {
			return err
		}
	}
	return nil
}

// CanGrantToRoles implements core.PermissionCatalog: actorID may hand new
// plugin permissions to each role of roleKeys, which is a change of that
// role (editableRole: within the actor's grantSet and, built-in or held,
// on its assignable list) to a role the actor may assign
// and does not hold itself (pluginGrantee, T59): the new permissions need
// not be held, so an actor naming its own role would get what
// PUT /roles/:id/permissions refuses. A critical permission takes a
// superuser actor besides (D3; an access key never is one). Super admin
// roles already hold every permission and are refused; unknown keys are
// invalid. Like every role write it bumps tx (serializing with other role
// changes) and locks the roles; the SyncPlugin that follows publishes the
// new version.
func (s *Service) CanGrantToRoles(ctx context.Context, tx pgx.Tx, actorID int64, roleKeys []string, critical bool) error {
	if roleKeys = dedupe(roleKeys); len(roleKeys) == 0 {
		return nil
	}
	if _, err := s.Bump(ctx, tx); err != nil {
		return err
	}
	roles, err := resolveRoles(ctx, tx, roleKeys)
	if err != nil {
		return err
	}
	actor, held, err := s.pluginGrantor(ctx, tx, actorID)
	if err != nil {
		return err
	}
	if critical && !actor.Superuser {
		return core.ErrPermissionDenied.WithMessage(t(ctx,
			"only the super admin may grant critical plugin permissions",
			"只有超级管理员能授予插件的关键权限")).WithDetails(map[string]any{"reason": "critical", "superuser": true})
	}
	for _, r := range roles {
		if r.superuser {
			return core.ErrPermissionDenied.WithMessage(t(ctx,
				"the super admin role holds every permission and is not granted plugin permissions",
				"超级管理员角色本来拥有全部权限，不能授予插件权限")).WithDetails(map[string]any{"role": r.key})
		}
		if _, err := s.editableRole(ctx, tx, actorID, r.id); err != nil {
			return err
		}
		if err := pluginGrantee(ctx, actor, held, r.key); err != nil {
			return err
		}
	}
	return nil
}

// PluginGrantees implements core.PermissionCatalog: CanGrantToRoles's
// rules (editDenial, granteeDenial) read-only, without a bump or locks, a
// refusal being a Denied entry; unknown keys are invalid.
func (s *Service) PluginGrantees(ctx context.Context, tx pgx.Tx, actorID int64, roleKeys []string) (core.PluginGrantees, error) {
	roleKeys = dedupe(roleKeys)
	rows, err := tx.Query(ctx, roleSelect+` WHERE r.key = ANY($1) ORDER BY r.key`, roleKeys)
	if err != nil {
		return core.PluginGrantees{}, err
	}
	roles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Role, error) { return scanRole(row) })
	if err != nil {
		return core.PluginGrantees{}, err
	}
	found := make([]string, len(roles))
	for i, r := range roles {
		found[i] = r.Key
	}
	if unknown := addedKeys(roleKeys, found); len(unknown) > 0 {
		return core.PluginGrantees{}, core.InvalidFields(core.FieldError{Field: "role_keys", Code: "unknown", Message: "unknown roles: " + strings.Join(unknown, ", ")})
	}
	actor, held, err := s.pluginGrantor(ctx, tx, actorID)
	if err != nil {
		return core.PluginGrantees{}, err
	}
	out := core.PluginGrantees{Superuser: actor.Superuser, Denied: map[string]string{}}
	for _, r := range roles {
		cur, err := s.RolesPermissions(ctx, tx, []string{r.Key})
		if err != nil {
			return core.PluginGrantees{}, err
		}
		if why := cmp.Or(editDenial(actor, cur, r), granteeDenial(actor, held, r.Key)); why != "" {
			out.Denied[r.Key] = why
		}
	}
	return out, nil
}

// pluginGrantor is actorID's grantSet with the keys of the roles it holds;
// the system (actor 0) is a superuser, as editableRole lets it through.
func (s *Service) pluginGrantor(ctx context.Context, tx pgx.Tx, actorID int64) (core.PermissionSet, []string, error) {
	if actorID == 0 {
		return core.PermissionSet{Superuser: true}, nil, nil
	}
	set, held, err := s.usersRoles(ctx, tx, []int64{actorID})
	return core.RestrictToAccessKey(ctx, actorID, set), held, err
}

// Why a role may not be handed new plugin permissions (granteeDenial).
const denyHeld = "held"

// granteeDenial is why actor may not hand new plugin permissions to role
// key, "" when it may (T59): the role must be on its assignable list and,
// but for a superuser, not one it holds. A list may name the holder's own
// role (only the assign_custom wildcard leaves it out), and the new
// permissions need not be held, so without the second check an actor could
// grow its own role.
func granteeDenial(actor core.PermissionSet, held []string, key string) string {
	switch {
	case !actor.CanAssign(key):
		return denyNotAssignable
	case !actor.Superuser && slices.Contains(held, key):
		return denyHeld
	}
	return ""
}

// pluginGrantee is granteeDenial as an error.
func pluginGrantee(ctx context.Context, actor core.PermissionSet, held []string, key string) error {
	switch granteeDenial(actor, held, key) {
	case denyNotAssignable:
		return notAssignable(ctx, key)
	case denyHeld:
		return core.ErrPermissionDenied.WithMessage(t(ctx,
			"you cannot grant plugin permissions to a role you hold",
			"不能把插件权限授予自己持有的角色")).WithDetails(map[string]any{"role": key})
	}
	return nil
}

// GrantPluginPermission implements core.PermissionCatalog; roles naming a
// scope the permission does not take are invalid.
func (s *Service) GrantPluginPermission(ctx context.Context, tx pgx.Tx, key string, roles map[string][]string) error {
	var scopeType string
	var scope manifest.PermissionScope
	err := tx.QueryRow(ctx, `SELECT scope_type, scope FROM permissions WHERE key = $1 AND source = 'plugin'`, key).Scan(&scopeType, &scope)
	switch {
	case store.IsNoRows(err):
		return nil // no plugin permission: nothing to grant
	case err != nil:
		return err
	}
	if err := checkScopes(key, readPluginScope(scopeType, &scope), roles); err != nil {
		return err
	}
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return err
	}
	if err := s.grantPlugin(ctx, tx, "review", key, roles); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// grantPlugin grants the plugin permission key to roles (role -> scope
// suffixes, "" every row, which makes the role's others redundant) but super
// admin ones (which hold every permission), audited with via and the
// granted key, its scope included. A removed key grants nothing. Callers
// check the scopes (checkScopes).
func (s *Service) grantPlugin(ctx context.Context, tx pgx.Tx, via, key string, roles map[string][]string) error {
	var roleKeys, scopes []string
	for _, role := range slices.Sorted(maps.Keys(roles)) {
		suffixes := slices.Compact(slices.Sorted(slices.Values(roles[role])))
		if slices.Contains(suffixes, "") {
			suffixes = []string{""}
		}
		for _, sfx := range suffixes {
			roleKeys, scopes = append(roleKeys, role), append(scopes, sfx)
		}
	}
	if len(roleKeys) == 0 {
		return nil
	}
	return s.syncGrant(ctx, tx, map[string]any{"via": via}, `
WITH g AS (
    SELECT r.id AS role_id, p.id AS permission_id, t.scope
    FROM unnest($1::text[], $2::text[]) AS t(role, scope)
    JOIN roles r ON r.key = t.role AND NOT r.superuser
    JOIN permissions p ON p.key = $3 AND p.source = 'plugin' AND p.status <> 'removed'),
ins AS (
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT role_id, permission_id FROM g WHERE scope = ''
    ON CONFLICT DO NOTHING RETURNING role_id, permission_id, ''::varchar AS scope),
ins_scoped AS (
    INSERT INTO role_permission_scopes (role_id, permission_id, scope)
    SELECT role_id, permission_id, scope FROM g WHERE scope <> ''
      AND NOT EXISTS (SELECT 1 FROM role_permissions f WHERE f.role_id = g.role_id AND f.permission_id = g.permission_id)
    ON CONFLICT DO NOTHING RETURNING role_id, permission_id, scope)
SELECT i.role_id, p.key || CASE WHEN i.scope = '' THEN '' ELSE ':' || i.scope END
FROM (SELECT * FROM ins UNION ALL SELECT * FROM ins_scoped) i JOIN permissions p ON p.id = i.permission_id`, roleKeys, scopes, key)
}

// SetPluginActive implements core.PermissionCatalog.
func (s *Service) SetPluginActive(ctx context.Context, tx pgx.Tx, pluginKey string, active bool) error {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return err
	}
	status := "disabled"
	if active {
		status = "active"
	}
	if _, err := tx.Exec(ctx, `UPDATE permissions SET status = $2 WHERE plugin_key = $1 AND status <> 'removed'`, pluginKey, status); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// DeletePlugin implements core.PermissionCatalog; role grants cascade.
func (s *Service) DeletePlugin(ctx context.Context, tx pgx.Tx, pluginKey string) error {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM permissions WHERE plugin_key = $1`, pluginKey); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}
