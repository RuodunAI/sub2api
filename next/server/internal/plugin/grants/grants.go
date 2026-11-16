// Package grants decides which host permission grants an instance of a
// plugin version runs with (CONTRACTS §5.7). plugin_permission_grants holds
// what the active version is granted; a consent to a version that is not
// active yet while another one runs is staged (plugin_staged_grants) and
// reaches instances of that version only, until the rollout commit
// promotes it or an abandoned upgrade withdraws it.
package grants

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// versionGrants selects (as g) the grant rows an instance of key@version
// runs with, in the columns of plugin_permission_grants a reader needs
// (permission, scope, status, plugin_version, granted_by, granted_at): the
// set staged for that version when there is one (it is complete), else the
// plugin's grants.
func versionGrants(db *gorm.DB, key, version string) *gorm.DB {
	staged := func() *gorm.DB {
		return db.Model(&model.PluginStagedGrant{}).Where("plugin_key = ? AND version = ?", key, version)
	}
	plugin := db.Model(&model.PluginPermissionGrant{}).Select("permission, scope, status, plugin_version, granted_by, granted_at").
		Where("plugin_key = ? AND NOT EXISTS (?)", key, staged().Select("1"))
	return db.Table("((?) UNION ALL (?)) AS g",
		staged().Select("permission, scope, status, version AS plugin_version, granted_by, granted_at"), plugin)
}

// VersionGrantsQuery selects the grant rows key@version runs with, in the
// columns permission, scope, status, plugin_version, granted_by and
// granted_at.
func VersionGrantsQuery(db *gorm.DB, key, version string) *gorm.DB {
	return versionGrants(db, key, version).Select("permission, scope, status, plugin_version, granted_by, granted_at")
}

// scopeRow is a (permission, scope) row of the grant tables.
type scopeRow struct {
	Permission string
	Scope      []byte
}

// Granted returns permission -> scope of what an instance of key@version is
// granted, limited to the host permissions its manifest declares: a grant
// made for another version never reaches it.
func Granted(ctx context.Context, q *gorm.DB, key, version string, declared []manifest.HostPermission) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(declared) == 0 {
		return out, nil
	}
	ids := make([]string, len(declared))
	for i, hp := range declared {
		ids[i] = hp.ID
	}
	var rows []scopeRow
	if err := grantedRows(store.G(ctx, q), key, version, ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r.Scope) == 0 {
			r.Scope = []byte("{}")
		}
		out[r.Permission] = r.Scope
	}
	return out, nil
}

// grantedRows selects the (permission, scope) rows key@version is granted among
// the permissions ids.
func grantedRows(db *gorm.DB, key, version string, ids []string) *gorm.DB {
	return versionGrants(db, key, version).Select("permission, scope").
		Where("status = 'granted' AND permission = ANY(?)", store.Strings(ids))
}

// Holds reports whether key@version is granted permission now, with exactly
// scope unless it is nil: the per-call check behind an instance's cached
// grants, and the rollout's check of db.schema before migrating. Every
// HostService call that needs a permission runs it
// (BenchmarkHotGrantHolds, gormtest/BASELINE.md).
func Holds(ctx context.Context, q *gorm.DB, key, version, permission string, scope []byte) (bool, error) {
	var one int
	err := holdsQuery(store.G(ctx, q), key, version, permission, scope).Select("1").Limit(1).Row().Scan(&one)
	if store.IsNoRows(err) {
		return false, nil
	}
	return err == nil, err
}

// holdsQuery selects the grant of permission to key@version, with exactly
// scope unless it is nil.
func holdsQuery(db *gorm.DB, key, version, permission string, scope []byte) *gorm.DB {
	q := versionGrants(db, key, version).Where("permission = ? AND status = 'granted'", permission)
	if scope != nil {
		q = q.Where("scope = ?::jsonb", store.Bytes(scope))
	}
	return q
}

// Decision is one host permission decision of a consent.
type Decision struct {
	Permission string
	Scope      []byte
	Status     string
}

// Record writes the decisions of a consent to key@version: staged while
// another version runs, else as the plugin's grants. Each decision is an
// upsert of its row, granted now.
func Record(ctx context.Context, tx *gorm.DB, key, version, manifestHash string, by *int64, staged bool, ds []Decision) error {
	table, versionColumn := model.PluginPermissionGrant{}.TableName(), "plugin_version"
	conflict := []clause.Column{{Name: "plugin_key"}, {Name: "permission"}}
	set := []string{"scope", "status", "plugin_version", "manifest_hash", "granted_by"}
	if staged {
		table, versionColumn = model.PluginStagedGrant{}.TableName(), "version"
		conflict = []clause.Column{{Name: "plugin_key"}, {Name: "version"}, {Name: "permission"}}
		set = []string{"scope", "status", "manifest_hash", "granted_by"}
	}
	upsert := clause.OnConflict{Columns: conflict, DoUpdates: append(clause.AssignmentColumns(set),
		clause.Assignment{Column: clause.Column{Name: "granted_at"}, Value: gorm.Expr("now()")})}
	g := store.G(ctx, tx)
	for _, d := range ds {
		if err := g.Table(table).Clauses(upsert).Create(map[string]any{"plugin_key": key, versionColumn: version,
			"manifest_hash": manifestHash, "granted_by": by, "permission": d.Permission, "scope": d.Scope,
			"status": d.Status, "granted_at": gorm.Expr("now()")}).Error; err != nil {
			return err
		}
	}
	return nil
}

// Promote makes the set staged for version the plugin's grants; the rollout
// commit calls it when version becomes active (nothing staged: no-op). It
// takes the set out and writes it over the plugin's grants, in the
// caller's transaction.
func Promote(ctx context.Context, tx *gorm.DB, key, version string) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	g := store.G(ctx, tx)
	staged, err := takeStaged(g, key, version)
	if err != nil || len(staged) == 0 {
		return err
	}
	return g.Table(model.PluginPermissionGrant{}.TableName()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_key"}, {Name: "permission"}},
		DoUpdates: clause.AssignmentColumns([]string{"scope", "status", "plugin_version", "manifest_hash", "granted_by", "granted_at"}),
	}).Create(pluginGrants(staged)).Error
}

// takeStaged deletes the set staged for key@version and returns its rows.
func takeStaged(g *gorm.DB, key, version string) ([]model.PluginStagedGrant, error) {
	var rows []model.PluginStagedGrant
	err := g.Clauses(clause.Returning{}).Where("plugin_key = ? AND version = ?", key, version).Delete(&rows).Error
	return rows, err
}

// pluginGrants are staged rows as plugin grant rows, every column written
// as it was (a NULL scope stays NULL).
func pluginGrants(staged []model.PluginStagedGrant) []map[string]any {
	out := make([]map[string]any, len(staged))
	for i, r := range staged {
		var scope any
		if r.Scope != nil {
			scope = store.Bytes(r.Scope)
		}
		out[i] = map[string]any{"plugin_key": r.PluginKey, "permission": r.Permission, "scope": scope, "status": r.Status,
			"plugin_version": r.Version, "manifest_hash": r.ManifestHash, "granted_by": r.GrantedBy, "granted_at": r.GrantedAt}
	}
	return out
}

// Withdraw takes back the set staged for version when its upgrade is
// abandoned (reason: the rollout's final phase): the version waits for a
// new consent, and the system audits plugin.grants.withdraw with the
// permissions that were granted. A revocation in the set is kept as a
// plugin grant row unless the plugin has one for that permission (it was
// revoked with it, or decided since), so it still stands (Revoked).
// Nothing staged: no-op.
func Withdraw(ctx context.Context, tx *gorm.DB, key, version, reason string) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	g := store.G(ctx, tx)
	gone, err := takeStaged(g, key, version)
	if err != nil || len(gone) == 0 {
		return err
	}
	var kept []model.PluginStagedGrant
	granted := []string{}
	for _, r := range gone {
		switch r.Status {
		case "revoked":
			kept = append(kept, r)
		case "granted":
			granted = append(granted, r.Permission)
		}
	}
	slices.Sort(granted)
	if len(kept) > 0 {
		if err := g.Table(model.PluginPermissionGrant{}.TableName()).Clauses(clause.OnConflict{DoNothing: true}).
			Create(pluginGrants(kept)).Error; err != nil {
			return err
		}
	}
	if err := awaitConsent(g, key, version).Error; err != nil {
		return err
	}
	return audit.Audit(ctx, tx, 0, "plugin.grants.withdraw", "plugin", key, map[string]any{
		"version": version, "reason": reason, "permissions": granted,
	})
}

// awaitConsent sends key@version, if approved, back to awaiting consent.
func awaitConsent(db *gorm.DB, key, version string) *gorm.DB {
	return db.Model(&model.PluginVersion{}).
		Where("plugin_key = ? AND version = ? AND consent_status = 'approved'", key, version).
		Update("consent_status", "awaiting_consent")
}

// Revoke marks permission revoked wherever key grants it: the plugin's
// grants and every staged set (an emergency revocation must not come back
// with the next activation). It reports whether a grant was revoked.
func Revoke(ctx context.Context, tx *gorm.DB, key, permission string, by *int64) (bool, error) {
	if err := store.RequireTx(tx); err != nil {
		return false, err
	}
	g := store.G(ctx, tx)
	var n int64
	for _, table := range []string{model.PluginPermissionGrant{}.TableName(), model.PluginStagedGrant{}.TableName()} {
		res := g.Table(table).Where("plugin_key = ? AND permission = ? AND status = 'granted'", key, permission).
			Updates(map[string]any{"status": "revoked", "granted_by": by, "granted_at": gorm.Expr("now()")})
		if res.Error != nil {
			return false, res.Error
		}
		n += res.RowsAffected
	}
	return n > 0, nil
}

// Narrow narrows permission to scope in every staged set of key that grants
// it a wider scope, as an administrator narrows the plugin's grant (the
// caller records that one): activating the version must not widen it again.
// A set granting a scope neither wider nor within scope keeps it: it is that
// version's own decision, on another request.
func Narrow(ctx context.Context, tx *gorm.DB, key, permission string, scope map[string]any, by *int64) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	// The scopes the staged sets of key grant permission, locked.
	granted, err := stagedScopes(store.G(ctx, tx).Model(&model.PluginStagedGrant{}).Select("version, scope").
		Where("plugin_key = ? AND permission = ? AND status = 'granted'", key, permission).Clauses(store.ForUpdate()), key, permission)
	if err != nil {
		return err
	}
	var versions []string
	for version, g := range granted {
		if pkg.ScopeWithin(scope, g) && !pkg.ScopeWithin(g, scope) {
			versions = append(versions, version)
		}
	}
	return grantVersions(ctx, tx, key, permission, scope, by, versions)
}

// Reinstate lifts the revocation of permission, granting it scope, in every
// staged set of key whose version requests at least scope: an
// administrator's grant, the counterpart of Revoke. A set whose version
// requests less stays revoked, so that version still runs without it.
func Reinstate(ctx context.Context, tx *gorm.DB, key, permission string, scope map[string]any, by *int64) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	// The staged sets of key that revoke permission, locked, with the scope
	// each one's version requests for it.
	requested, err := stagedScopes(store.G(ctx, tx).Table("plugin_staged_grants AS s").Select("s.version, hp->'scope' AS scope").
		Joins("JOIN plugin_versions v ON v.plugin_key = s.plugin_key AND v.version = s.version").
		Joins("CROSS JOIN LATERAL jsonb_array_elements(v.manifest->'hostPermissions') hp").
		Where("s.plugin_key = ? AND s.permission = ? AND s.status = 'revoked' AND hp->>'id' = ?", key, permission, permission).
		Clauses(store.ForUpdate().Of("s")), key, permission)
	if err != nil {
		return err
	}
	var versions []string
	for version, r := range requested {
		if pkg.ScopeWithin(scope, r) {
			versions = append(versions, version)
		}
	}
	return grantVersions(ctx, tx, key, permission, scope, by, versions)
}

// stagedScopes reads version -> scope from the (version, scope) rows of q;
// a NULL scope (a host permission requesting none) is nil, a scope that is
// no JSON object an error.
func stagedScopes(q *gorm.DB, key, permission string) (map[string]map[string]any, error) {
	var rows []struct {
		Version string
		Scope   []byte
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, r := range rows {
		var s map[string]any
		if r.Scope != nil {
			if err := json.Unmarshal(r.Scope, &s); err != nil {
				return nil, fmt.Errorf("scope of %s@%s %s: %w", key, r.Version, permission, err)
			}
		}
		out[r.Version] = s
	}
	return out, nil
}

// grantVersions grants permission with scope in the staged sets of key for
// versions.
func grantVersions(ctx context.Context, tx *gorm.DB, key, permission string, scope map[string]any, by *int64, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	b, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	return grantStaged(store.G(ctx, tx), key, permission, versions, b, by).Error
}

// grantStaged grants permission with scope, a JSON document, in the staged
// sets of key for versions.
func grantStaged(db *gorm.DB, key, permission string, versions []string, scope []byte, by *int64) *gorm.DB {
	return db.Model(&model.PluginStagedGrant{}).
		Where("plugin_key = ? AND permission = ? AND version = ANY(?)", key, permission, store.Strings(versions)).
		Updates(map[string]any{"status": "granted", "scope": scope, "granted_by": by, "granted_at": gorm.Expr("now()")})
}

// Revoked reports the host permissions of key whose latest decision, over
// the plugin's grants and every staged set, is a revocation: an
// administrator revoked them and no consent has granted them since. A
// consent grants them only when it names them (CONTRACTS §5.7).
func Revoked(ctx context.Context, q *gorm.DB, key string) (map[string]bool, error) {
	var perms []string
	if err := revoked(store.G(ctx, q), key).Scan(&perms).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(perms))
	for _, p := range perms {
		out[p] = true
	}
	return out, nil
}

// revoked selects the permissions of key whose latest decision is a
// revocation: the latest row of each permission over the plugin's grants and
// the staged sets.
func revoked(db *gorm.DB, key string) *gorm.DB {
	latest := db.Table("((?) UNION ALL (?)) AS d",
		db.Model(&model.PluginPermissionGrant{}).Select("permission, status, granted_at").Where("plugin_key = ?", key),
		db.Model(&model.PluginStagedGrant{}).Select("permission, status, granted_at").Where("plugin_key = ?", key)).
		Select("DISTINCT ON (permission) permission, status").Order("permission, granted_at DESC, status = 'revoked' DESC")
	return db.Table("(?) AS latest", latest).Select("permission").Where("status = 'revoked'")
}
