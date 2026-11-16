package authz

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// runningPluginStatuses are the plugins.status values (install.StatusEnabled,
// install.StatusUpgrading) under which a plugin serves traffic; a test keeps
// the list in step with the install package.
const runningPluginStatuses = `'enabled','upgrading'`

// SyncPlugin implements core.PermissionCatalog. A new permission follows the
// plugin: active while the plugin runs, disabled otherwise (SetPluginActive
// turns it on at enable), and is granted to its GrantRoles (the consent
// resolved inheritance into them, install.planRoleGrants), each grant
// audited as authz.sync.grant. Existing ones keep their status and role
// grants but the scoped ones their new scope no longer takes (pruneScopes);
// a removed one declared again revives with the plugin's status and the
// grants it had, never its GrantRoles (D6), audited as authz.sync.revive.
// A Staged definition (declared by a version approved while another runs,
// not by the running one) is created staged, granting nothing whatever the
// plugin's status, and neither revives a removed key nor prunes its scope
// grants: the version's activation, syncing again, does that and turns the
// staged keys it declares on, keeping what its consent granted them. Keys
// no longer declared are marked removed last, audited as
// authz.sync.remove: their grants stay, granting nothing, until the key
// comes back. Staged keys stay as they are until that version activates or
// another consent discards them (DiscardStaged).
func (s *Service) SyncPlugin(ctx context.Context, tx *gorm.DB, pluginKey string, defs []core.PermissionDef) error {
	if pluginKey == "" {
		return core.ErrInvalidArgument.WithMessage("plugin key is required")
	}
	v, err := s.bumpPolicy(ctx, tx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(defs))
	var created []core.PermissionDef
	var revived []string
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
		inserted, wasRemoved, err := upsertPluginPermission(ctx, tx, pluginKey, local, d)
		if err != nil {
			return err
		}
		if wasRemoved && !d.Staged {
			revived = append(revived, key)
		}
		if inserted {
			created = append(created, d)
			continue
		}
		// A staged key's scope grants are pruned at activation, if at all.
		if !d.Staged {
			if err := s.pruneScopes(ctx, tx, key, core.ReadPluginScope(d.ScopeType, d.Scope)); err != nil {
				return err
			}
		}
	}
	for _, d := range created {
		if err := s.grantPlugin(ctx, tx, "default", d.Key, d.GrantRoles); err != nil {
			return err
		}
	}
	removed := []string{}
	if err := removeUndeclared(store.G(ctx, tx), pluginKey, keys).Scan(&removed).Error; err != nil {
		return err
	}
	if err := s.auditKeyStatus(ctx, tx, "authz.sync.revive", pluginKey, revived); err != nil {
		return err
	}
	if err := s.auditKeyStatus(ctx, tx, "authz.sync.remove", pluginKey, removed); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// removeUndeclared marks removed the permissions of pluginKey but keys,
// staged ones aside, and returns their keys.
func removeUndeclared(db *gorm.DB, pluginKey string, keys []string) *gorm.DB {
	return db.Raw(`UPDATE permissions SET status = 'removed'
WHERE plugin_key = ? AND status NOT IN ('removed', 'staged') AND key <> ALL(?) RETURNING key`, pluginKey, store.Strings(keys))
}

// auditKeyStatus records keys of pluginKey marked removed (authz.sync.remove),
// declared again (authz.sync.revive) or discarded staged
// (authz.sync.discard) with the roles holding each: their grants stop or
// start counting, or go, with no role write behind it. Nothing is
// written for no keys.
func (s *Service) auditKeyStatus(ctx context.Context, tx *gorm.DB, action, pluginKey string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	grants, err := s.PermissionHolders(ctx, tx, keys)
	if err != nil {
		return err
	}
	holders := map[string][]string{}
	for key, hs := range grants {
		for _, h := range hs {
			if !slices.Contains(holders[key], h.Role) {
				holders[key] = append(holders[key], h.Role)
			}
		}
	}
	slices.Sort(keys)
	return s.audit(ctx, tx, 0, action, "plugin", pluginKey, map[string]any{"keys": keys, "holders": holders})
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
	ps := core.ReadPluginScope(d.ScopeType, d.Scope)
	if err := checkScopes(d.Key, ps, d.Defaults); err != nil {
		return err
	}
	return checkScopes(d.Key, ps, d.GrantRoles)
}

// checkScopes fails (400 invalid) unless every suffix of roles is a
// canonical scope of key that ps takes ("" every row always is).
func checkScopes(key string, ps manifest.PermissionScope, roles map[string][]string) error {
	var invalid []string
	for _, suffixes := range roles {
		for _, sfx := range suffixes {
			if sfx == "" {
				continue
			}
			if c, ok := core.PluginScopeSuffix(key, sfx); !ok || c != sfx || !ps.Takes(sfx) {
				invalid = append(invalid, withSuffix(key, sfx))
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
// local its plugin-local part), reporting whether it was created and
// whether it was removed before (it revives). sensitive
// is still written (risk <> 'low') for the previous core, should it run
// again; this one never reads it. A key of another owner is a conflict.
func upsertPluginPermission(ctx context.Context, tx *gorm.DB, pluginKey, local string, d core.PermissionDef) (inserted, wasRemoved bool, err error) {
	label := d.Label
	if label.Get("en") == "" {
		label = core.LocalizedText{"en": local}
	}
	var scope manifest.PermissionScope
	if d.Scope != nil {
		scope = *d.Scope
	}
	err = upsertPermission(store.G(ctx, tx), pluginPermissionRow{
		key: d.Key, module: manifest.PermissionModule(pluginKey), label: label, description: orEmpty(d.Description),
		pluginKey: pluginKey, risk: d.Risk, sort: d.Sort, scopeType: d.ScopeType, scope: scope, group: d.Group,
		groupLabel: orEmpty(d.GroupLabel), groupSort: d.GroupSort, requires: d.Requires, defaults: orEmpty(d.Defaults), staged: d.Staged,
	}).Row().Scan(&inserted, &wasRemoved)
	if store.IsNoRows(err) {
		return false, false, core.ErrConflict.WithMessage("permission " + d.Key + " belongs to another owner")
	}
	return inserted, wasRemoved, err
}

// pluginPermissionRow is what upsertPermission writes of a plugin
// permission.
type pluginPermissionRow struct {
	key, module, pluginKey, risk, scopeType, group string
	label, description, groupLabel                 core.LocalizedText
	sort, groupSort                                int
	scope                                          manifest.PermissionScope
	requires                                       []string
	defaults                                       map[string][]string
	staged                                         bool
}

// upsertPermission inserts or updates the plugin permission p unless its
// key belongs to another owner (no row then), returning whether it was
// inserted and whether it was removed before. prev reads the row as it was
// before the upsert (the statement's snapshot).
func upsertPermission(db *gorm.DB, p pluginPermissionRow) *gorm.DB {
	return db.Raw(`
WITH prev AS (SELECT status FROM permissions WHERE key = ?)
INSERT INTO permissions (key, module, label, description, source, plugin_key, risk, sensitive, status, sort,
    scope_type, scope, group_key, group_label, group_sort, requires, defaults)
VALUES (?, ?, ?, ?, 'plugin', ?::text, ?::varchar, ?::varchar <> 'low',
    CASE WHEN ?::bool THEN 'staged'
         WHEN EXISTS (SELECT 1 FROM plugins WHERE key = ?::text AND status IN (`+runningPluginStatuses+`)) THEN 'active'
         ELSE 'disabled' END,
    ?, ?, ?, ?, ?, ?, COALESCE(?::text[], '{}'), ?)
ON CONFLICT (key) DO UPDATE SET module = EXCLUDED.module, label = EXCLUDED.label,
    description = EXCLUDED.description, risk = EXCLUDED.risk, sensitive = EXCLUDED.sensitive,
    sort = EXCLUDED.sort, scope_type = EXCLUDED.scope_type, scope = EXCLUDED.scope,
    group_key = EXCLUDED.group_key, group_label = EXCLUDED.group_label, group_sort = EXCLUDED.group_sort,
    requires = EXCLUDED.requires, defaults = EXCLUDED.defaults,
    -- a removed key declared again revives, and a staged one turns on, with
    -- the plugin's status, unless staged still; others keep theirs
    status = CASE WHEN permissions.status IN ('removed', 'staged') AND NOT ?::bool THEN EXCLUDED.status ELSE permissions.status END
    WHERE permissions.plugin_key = EXCLUDED.plugin_key
RETURNING (xmax = 0), COALESCE((SELECT status = 'removed' FROM prev), false)`,
		p.key,
		p.key, p.module, p.label, p.description, p.pluginKey, p.risk, p.risk,
		p.staged,
		p.pluginKey,
		p.sort, p.scopeType, p.scope, p.group, p.groupLabel, p.groupSort, store.NullStrings(p.requires), p.defaults,
		p.staged)
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
func (s *Service) pruneScopes(ctx context.Context, tx *gorm.DB, key string, ps manifest.PermissionScope) error {
	db := store.G(ctx, tx)
	var scopes store.Strings
	if err := grantedScopes(db, key).Row().Scan(&scopes); err != nil {
		return err
	}
	stale := slices.DeleteFunc(scopes, ps.Takes)
	if len(stale) == 0 {
		return nil
	}
	gone, err := casbinrule.RevokeScopes(db, key, stale)
	if err != nil {
		return err
	}
	for _, g := range gone {
		if err := s.audit(ctx, tx, 0, "authz.sync.revoke", "role", g.RoleID, map[string]any{"via": "scope_changed", "key": g.Key}); err != nil {
			return err
		}
	}
	return nil
}

// grantedScopes selects the distinct scopes (canonical suffixes, every row
// aside) the permission key is granted at, as one array.
func grantedScopes(db *gorm.DB, key string) *gorm.DB {
	return db.Raw(`SELECT COALESCE(array_agg(DISTINCT g.scope), '{}') FROM (`+casbinrule.Grants+`) g WHERE g.action = ? AND g.scope <> ''`, key)
}

// CanGrantToRoles implements core.PermissionCatalog: actorID may hand new
// plugin permissions, of risk at most risk, to each role of roleKeys, which
// is a change of that role (editableRole: within the actor's grantSet and,
// built-in or held, on its assignable list) to a role the actor may assign
// and does not hold itself (pluginGrantee, T59): the new permissions need
// not be held, so an actor naming its own role would get what
// PUT /roles/:id/permissions refuses. A critical permission takes a
// superuser actor besides (D3; an access key never is one). Super admin
// roles already hold every permission and are refused; unknown keys are
// invalid. Like every role write it bumps tx (serializing with other role
// changes) and locks the roles; the SyncPlugin that follows publishes the
// new version.
func (s *Service) CanGrantToRoles(ctx context.Context, tx *gorm.DB, actorID int64, roleKeys []string, risk string) error {
	if roleKeys = dedupe(roleKeys); len(roleKeys) == 0 {
		return nil
	}
	if _, err := s.bumpPolicy(ctx, tx); err != nil {
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
	if risk == manifest.RiskCritical && !actor.Superuser {
		return core.ErrPermissionDenied.WithMessage(t(ctx,
			"only the super admin may grant critical plugin permissions",
			"只有超级管理员能授予插件的关键权限")).WithDetails(map[string]any{"reason": core.GrantDenyCritical, "superuser": true})
	}
	for _, r := range roles {
		if r.superuser {
			return core.ErrPermissionDenied.WithMessage(t(ctx,
				"the super admin role holds every permission and is not granted plugin permissions",
				"超级管理员角色本来拥有全部权限，不能授予插件权限")).WithDetails(map[string]any{"role": r.key})
		}
		if _, err := s.editableRole(ctx, tx, actorID, r.id, nil); err != nil {
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
func (s *Service) PluginGrantees(ctx context.Context, tx *gorm.DB, actorID int64, roleKeys []string) (core.PluginGrantees, error) {
	roleKeys = dedupe(roleKeys)
	roles, err := readRoles(rolesByKeys(store.G(ctx, tx), roleKeys))
	if err != nil {
		return core.PluginGrantees{}, err
	}
	found := make([]string, len(roles))
	for i, r := range roles {
		found[i] = r.Key
	}
	if unknown := keysNotIn(roleKeys, found); len(unknown) > 0 {
		return core.PluginGrantees{}, core.InvalidFields(core.FieldError{Field: "role_keys", Code: "unknown", Message: "unknown roles: " + strings.Join(unknown, ", ")})
	}
	actor, held, err := s.pluginGrantor(ctx, tx, actorID)
	if err != nil {
		return core.PluginGrantees{}, err
	}
	sets, err := s.eachRolePermissions(ctx, tx, found)
	if err != nil {
		return core.PluginGrantees{}, err
	}
	out := core.PluginGrantees{Superuser: actor.Superuser, Denied: map[string]string{}}
	for _, r := range roles {
		edit, _ := editDenial(actor, sets[r.Key], r)
		if why := cmp.Or(edit, granteeDenial(actor, held, r.Key)); why != "" {
			out.Denied[r.Key] = why
		}
	}
	return out, nil
}

// pluginGrantor is actorID's grantSet with the keys of the roles it holds;
// the system (actor 0) is a superuser, as editableRole lets it through.
func (s *Service) pluginGrantor(ctx context.Context, tx *gorm.DB, actorID int64) (core.PermissionSet, []string, error) {
	if actorID == 0 {
		return core.PermissionSet{Superuser: true}, nil, nil
	}
	set, held, err := s.usersRoles(ctx, tx, []int64{actorID})
	return core.RestrictToAccessKey(ctx, actorID, set), held, err
}

// granteeDenial is why actor may not hand new plugin permissions to role
// key, "" when it may (T59): the role must be on its assignable list and,
// but for a superuser, not one it holds. A list may name the holder's own
// role (only the assign_custom wildcard leaves it out), and the new
// permissions need not be held, so without the second check an actor could
// grow its own role.
func granteeDenial(actor core.PermissionSet, held []string, key string) string {
	switch {
	case !actor.CanAssign(key):
		return core.GrantDenyNotAssignable
	case !actor.Superuser && slices.Contains(held, key):
		return core.GrantDenyHeld
	}
	return ""
}

// pluginGrantee is granteeDenial as an error.
func pluginGrantee(ctx context.Context, actor core.PermissionSet, held []string, key string) error {
	switch granteeDenial(actor, held, key) {
	case core.GrantDenyNotAssignable:
		return notAssignable(ctx, key)
	case core.GrantDenyHeld:
		return core.ErrPermissionDenied.WithMessage(t(ctx,
			"you cannot grant plugin permissions to a role you hold",
			"不能把插件权限授予自己持有的角色")).WithDetails(map[string]any{"role": key})
	}
	return nil
}

// GrantPluginPermission implements core.PermissionCatalog; roles naming a
// scope the permission does not take are invalid.
func (s *Service) GrantPluginPermission(ctx context.Context, tx *gorm.DB, key string, roles map[string][]string) error {
	var scopeType string
	var scope store.JSON[manifest.PermissionScope]
	err := pluginPermissionScope(store.G(ctx, tx), key).Row().Scan(&scopeType, &scope)
	switch {
	case store.IsNoRows(err):
		return nil // no plugin permission: nothing to grant
	case err != nil:
		return err
	}
	if err := checkScopes(key, core.ReadPluginScope(scopeType, &scope.V), roles); err != nil {
		return err
	}
	v, err := s.bumpPolicy(ctx, tx)
	if err != nil {
		return err
	}
	if err := s.grantPlugin(ctx, tx, "review", key, roles); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// pluginPermissionScope selects the scope type and scope of the plugin
// permission key.
func pluginPermissionScope(db *gorm.DB, key string) *gorm.DB {
	return db.Model(&model.Permission{}).Select("scope_type", "scope").Where("key = ? AND source = 'plugin'", key)
}

// grantPlugin grants the plugin permission key to roles (role -> scope
// suffixes, "" every row, which makes the role's others redundant) but super
// admin ones (which hold every permission), audited with via and the
// granted key, its scope included. A removed key grants nothing; a scoped
// grant next to the role's every row grant is redundant and left out.
// Callers check the scopes (checkScopes).
func (s *Service) grantPlugin(ctx context.Context, tx *gorm.DB, via, key string, roles map[string][]string) error {
	var grants []casbinrule.Grant
	for _, role := range slices.Sorted(maps.Keys(roles)) {
		for _, sfx := range manifest.CanonicalSuffixes(roles[role]) {
			grants = append(grants, casbinrule.Grant{Role: role, Action: key, Suffix: sfx})
		}
	}
	added, err := casbinrule.AddGrants(store.G(ctx, tx), grants, casbinrule.Filter{Source: "plugin", Skip: []string{"removed"}, NoSuperuser: true})
	if err != nil {
		return err
	}
	return s.syncGrant(ctx, tx, map[string]any{"via": via}, added)
}

// PluginPermissions implements core.PermissionCatalog.
func (s *Service) PluginPermissions(ctx context.Context, q *gorm.DB, pluginKey string) ([]core.PluginPermission, error) {
	var rows []pluginPermissionRead
	if err := pluginPermissions(store.G(ctx, q), pluginKey).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]core.PluginPermission, len(rows))
	for i, r := range rows {
		out[i] = core.PluginPermission{Key: r.Key, Label: r.Label.V, Status: r.Status, Risk: r.Risk,
			ScopeType: r.ScopeType, Scope: core.ReadPluginScope(r.ScopeType, &r.Scope.V)}
	}
	return out, nil
}

// pluginPermissionRead is a permission as PluginPermissions reads it.
type pluginPermissionRead struct {
	Key       string                               `gorm:"column:key"`
	Status    string                               `gorm:"column:status"`
	Risk      string                               `gorm:"column:risk"`
	ScopeType string                               `gorm:"column:scope_type"`
	Label     store.JSON[core.LocalizedText]       `gorm:"column:label"`
	Scope     store.JSON[manifest.PermissionScope] `gorm:"column:scope"`
}

// permissionsOfPlugin are the permissions of the plugin pluginKey.
func permissionsOfPlugin(db *gorm.DB, pluginKey string) *gorm.DB {
	return db.Model(&model.Permission{}).Where("plugin_key = ?", pluginKey)
}

// pluginPermissions selects the permissions of the plugin pluginKey, by
// sort and key.
func pluginPermissions(db *gorm.DB, pluginKey string) *gorm.DB {
	return permissionsOfPlugin(db, pluginKey).Select("key", "label", "status", "risk", "scope_type", "scope").Order("sort, key")
}

// PermissionHolders implements core.PermissionCatalog: a role holds a key
// by its grants of every row (scope "") or of each scope, whatever the
// permission's status.
func (s *Service) PermissionHolders(ctx context.Context, q *gorm.DB, keys []string) (map[string][]core.PermissionHolder, error) {
	out := map[string][]core.PermissionHolder{}
	if len(keys) == 0 {
		return out, nil
	}
	var key string
	var h core.PermissionHolder
	err := store.EachRow(holderRows(store.G(ctx, q), keys), func(rows *sql.Rows) error {
		if err := rows.Scan(&key, &h.Role, &h.Scope); err != nil {
			return err
		}
		out[key] = append(out[key], h)
		return nil
	})
	return out, err
}

// holderRows selects the (key, role, scope) grants of the permissions
// keys, superuser roles aside, in that order.
func holderRows(db *gorm.DB, keys []string) *gorm.DB {
	return db.Raw(`
SELECT g.action, g.role, g.scope FROM (`+casbinrule.Grants+`) g JOIN roles r ON r.key = g.role AND NOT r.superuser
WHERE g.action = ANY(?)
ORDER BY 1, 2, 3`, store.Strings(keys))
}

// SetPluginActive implements core.PermissionCatalog.
func (s *Service) SetPluginActive(ctx context.Context, tx *gorm.DB, pluginKey string, active bool) error {
	v, err := s.bumpPolicy(ctx, tx)
	if err != nil {
		return err
	}
	status := "disabled"
	if active {
		status = "active"
	}
	if err := setPluginStatus(store.G(ctx, tx), pluginKey, status).Error; err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// setPluginStatus sets the status of the plugin's permissions, removed and
// staged ones aside.
func setPluginStatus(db *gorm.DB, pluginKey, status string) *gorm.DB {
	return db.Model(&model.Permission{}).Where("plugin_key = ? AND status NOT IN ('removed', 'staged')", pluginKey).Update("status", status)
}

// DiscardStaged implements core.PermissionCatalog: the plugin's staged keys
// and their grants go, audited as authz.sync.discard with the roles that
// held them; nothing happens without any.
func (s *Service) DiscardStaged(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	db := store.G(ctx, tx)
	var keys []string
	if err := lockStaged(db, pluginKey).Pluck("key", &keys).Error; err != nil || len(keys) == 0 {
		return err
	}
	v, err := s.bumpPolicy(ctx, tx)
	if err != nil {
		return err
	}
	if err := s.auditKeyStatus(ctx, tx, "authz.sync.discard", pluginKey, keys); err != nil {
		return err
	}
	if err := deletePermissions(db, keys); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}

// lockStaged selects the staged permissions of the plugin, by key, and
// locks them.
func lockStaged(db *gorm.DB, pluginKey string) *gorm.DB {
	return db.Model(&model.Permission{}).Where("plugin_key = ? AND status = 'staged'", pluginKey).Order("key").Clauses(store.ForUpdate())
}

// deletePermissions deletes the permissions keys with their grants (no
// foreign key cascades them: casbin_rule names a permission by key).
func deletePermissions(db *gorm.DB, keys []string) error {
	if err := casbinrule.DropPermissions(db, keys); err != nil {
		return err
	}
	return db.Where("key = ANY(?)", store.Strings(keys)).Delete(&model.Permission{}).Error
}

// DeletePlugin implements core.PermissionCatalog: the plugin's permissions
// go with their grants. Whoever deletes the plugin's row calls it first:
// the row's foreign key cascades to permissions, not to casbin_rule.
func (s *Service) DeletePlugin(ctx context.Context, tx *gorm.DB, pluginKey string) error {
	v, err := s.bumpPolicy(ctx, tx)
	if err != nil {
		return err
	}
	db := store.G(ctx, tx)
	var keys []string
	if err := permissionsOfPlugin(db, pluginKey).Pluck("key", &keys).Error; err != nil {
		return err
	}
	if err := deletePermissions(db, keys); err != nil {
		return err
	}
	s.committedLater(v)
	return nil
}
