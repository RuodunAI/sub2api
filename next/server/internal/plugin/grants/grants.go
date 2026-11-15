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
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// VersionRows is a FROM item (alias g) of the grant rows an instance of
// key@version ($1, $2) runs with, in the columns of plugin_permission_grants
// a reader needs: the set staged for that version when there is one (it is
// complete), else the plugin's grants.
const VersionRows = `(
	SELECT permission, scope, status, version AS plugin_version, granted_by, granted_at
	FROM plugin_staged_grants WHERE plugin_key = $1 AND version = $2
	UNION ALL
	SELECT permission, scope, status, plugin_version, granted_by, granted_at
	FROM plugin_permission_grants WHERE plugin_key = $1
	  AND NOT EXISTS (SELECT 1 FROM plugin_staged_grants WHERE plugin_key = $1 AND version = $2)) g`

// versionRows is VersionRows for GORM, which numbers the placeholders
// itself: the parameters are named (@key, @version) since each appears
// more than once. It is derived so that the two forms cannot drift apart.
var versionRows = strings.NewReplacer("$1", "@key", "$2", "@version").Replace(VersionRows)

// VersionGrantsQuery selects the grant rows key@version runs with (see
// VersionRows), in the columns permission, scope, status, plugin_version,
// granted_by and granted_at: the GORM form of reading VersionRows whole.
func VersionGrantsQuery(db *gorm.DB, key, version string) *gorm.DB {
	return db.Raw(`SELECT permission, scope, status, plugin_version, granted_by, granted_at FROM `+versionRows,
		map[string]any{"key": key, "version": version})
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
	return db.Raw(`SELECT permission, scope FROM `+versionRows+`
		WHERE status = 'granted' AND permission = ANY(@ids)`,
		map[string]any{"key": key, "version": version, "ids": store.Strings(ids)})
}

// Holds reports whether key@version is granted permission now, with exactly
// scope unless it is nil: the per-call check behind an instance's cached
// grants, and the rollout's check of db.schema before migrating. Every
// HostService call that needs a permission runs it, so it is positional Raw
// SQL scanned through Row, the GORM form that allocates least
// (BenchmarkHotGrantHolds, gormtest/BASELINE.md).
func Holds(ctx context.Context, q *gorm.DB, key, version, permission string, scope []byte) (bool, error) {
	var ok bool
	err := holdsQuery(store.G(ctx, q), key, version, permission, scope).Row().Scan(&ok)
	return ok, err
}

// holdsSQL is Holds's statement over VersionRows with positional
// placeholders: named ones (versionRows) would cost a map and its parsing on
// every call. Its key and version arguments therefore follow VersionRows's
// placeholder order, $1 $2 $1 $1 $2 (TestVersionRowsPlaceholderOrder).
var holdsSQL = `SELECT EXISTS (SELECT 1 FROM ` + strings.NewReplacer("$1", "?", "$2", "?").Replace(VersionRows) + `
		WHERE permission = ? AND status = 'granted' AND (?::jsonb IS NULL OR scope = ?::jsonb))`

// holdsQuery is Holds's statement. scope is a store.Bytes: nil is NULL (any
// scope), and the bytes stay one parameter after "(". The repeated
// arguments are boxed once each.
func holdsQuery(db *gorm.DB, key, version, permission string, scope []byte) *gorm.DB {
	k, v, s := any(key), any(version), any(store.Bytes(scope))
	return db.Raw(holdsSQL, k, v, k, k, v, permission, s, s)
}

// Decision is one host permission decision of a consent.
type Decision struct {
	Permission string
	Scope      []byte
	Status     string
}

// Record writes the decisions of a consent to key@version: staged while
// another version runs, else as the plugin's grants.
func Record(ctx context.Context, tx *gorm.DB, key, version, manifestHash string, by *int64, staged bool, ds []Decision) error {
	sql := recordSQL(staged)
	g := store.G(ctx, tx)
	for _, d := range ds {
		if err := g.Exec(sql, key, version, manifestHash, by, d.Permission, d.Scope, d.Status).Error; err != nil {
			return err
		}
	}
	return nil
}

// recordSQL upserts one decision (key, version, manifest hash, by,
// permission, scope, status) into the staged set of the version or the
// plugin's grants.
func recordSQL(staged bool) string {
	if staged {
		return `INSERT INTO plugin_staged_grants (plugin_key, version, manifest_hash, granted_by, permission, scope, status, granted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, now())
			ON CONFLICT (plugin_key, version, permission) DO UPDATE SET
			  scope = EXCLUDED.scope, status = EXCLUDED.status, manifest_hash = EXCLUDED.manifest_hash,
			  granted_by = EXCLUDED.granted_by, granted_at = now()`
	}
	return `INSERT INTO plugin_permission_grants (plugin_key, plugin_version, manifest_hash, granted_by, permission, scope, status, granted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, now())
		ON CONFLICT (plugin_key, permission) DO UPDATE SET
		  scope = EXCLUDED.scope, status = EXCLUDED.status, plugin_version = EXCLUDED.plugin_version,
		  manifest_hash = EXCLUDED.manifest_hash, granted_by = EXCLUDED.granted_by, granted_at = now()`
}

// Promote makes the set staged for version the plugin's grants; the rollout
// commit calls it when version becomes active (nothing staged: no-op).
func Promote(ctx context.Context, tx *gorm.DB, key, version string) error {
	return store.G(ctx, tx).Exec(promoteSQL, key, version).Error
}

// promoteSQL moves the set staged for key@version into the plugin's grants.
const promoteSQL = `
		WITH staged AS (DELETE FROM plugin_staged_grants WHERE plugin_key = ? AND version = ? RETURNING *)
		INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash, granted_by, granted_at)
		SELECT plugin_key, permission, scope, status, version, manifest_hash, granted_by, granted_at FROM staged
		ON CONFLICT (plugin_key, permission) DO UPDATE SET
		  scope = EXCLUDED.scope, status = EXCLUDED.status, plugin_version = EXCLUDED.plugin_version,
		  manifest_hash = EXCLUDED.manifest_hash, granted_by = EXCLUDED.granted_by, granted_at = EXCLUDED.granted_at`

// Withdraw takes back the set staged for version when its upgrade is
// abandoned (reason: the rollout's final phase): the version waits for a
// new consent, and the system audits plugin.grants.withdraw with the
// permissions that were granted. A revocation in the set is kept as a
// plugin grant row unless the plugin has one for that permission (it was
// revoked with it, or decided since), so it still stands (Revoked).
// Nothing staged: no-op.
func Withdraw(ctx context.Context, tx *gorm.DB, key, version, reason string) error {
	var (
		n       int
		granted store.Strings
	)
	g := store.G(ctx, tx)
	if err := withdrawn(g, key, version).Row().Scan(&n, &granted); err != nil || n == 0 {
		return err
	}
	if err := awaitConsent(g, key, version).Error; err != nil {
		return err
	}
	return audit.Audit(ctx, tx, 0, "plugin.grants.withdraw", "plugin", key, map[string]any{
		"version": version, "reason": reason, "permissions": []string(granted),
	})
}

// withdrawn deletes the set staged for key@version, keeps its revocations
// as plugin grant rows, and reads how many rows it held and the
// permissions they granted.
func withdrawn(db *gorm.DB, key, version string) *gorm.DB {
	return db.Raw(`
		WITH gone AS (DELETE FROM plugin_staged_grants WHERE plugin_key = ? AND version = ? RETURNING *),
		kept AS (INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash, granted_by, granted_at)
			SELECT plugin_key, permission, scope, status, version, manifest_hash, granted_by, granted_at FROM gone WHERE status = 'revoked'
			ON CONFLICT (plugin_key, permission) DO NOTHING)
		SELECT count(*), COALESCE(array_agg(permission ORDER BY permission) FILTER (WHERE status = 'granted'), '{}') FROM gone`,
		key, version)
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
	var n int
	err := revoke(store.G(ctx, tx), key, permission, by).Row().Scan(&n)
	return n > 0, err
}

// revoke revokes permission in the plugin's grants and the staged sets of
// key and reads how many grants it revoked.
func revoke(db *gorm.DB, key, permission string, by *int64) *gorm.DB {
	return db.Raw(`
		WITH active AS (UPDATE plugin_permission_grants SET status = 'revoked', granted_by = @by, granted_at = now()
			WHERE plugin_key = @key AND permission = @permission AND status = 'granted' RETURNING 1),
		staged AS (UPDATE plugin_staged_grants SET status = 'revoked', granted_by = @by, granted_at = now()
			WHERE plugin_key = @key AND permission = @permission AND status = 'granted' RETURNING 1)
		SELECT (SELECT count(*) FROM active) + (SELECT count(*) FROM staged)`,
		map[string]any{"key": key, "permission": permission, "by": by})
}

// Narrow narrows permission to scope in every staged set of key that grants
// it a wider scope, as an administrator narrows the plugin's grant (the
// caller records that one): activating the version must not widen it again.
// A set granting a scope neither wider nor within scope keeps it: it is that
// version's own decision, on another request.
func Narrow(ctx context.Context, tx *gorm.DB, key, permission string, scope map[string]any, by *int64) error {
	granted, err := stagedScopes(ctx, tx, narrowedSQL, key, permission)
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

// narrowedSQL locks and reads the scopes the staged sets of key grant
// permission.
const narrowedSQL = `SELECT version, scope FROM plugin_staged_grants
		WHERE plugin_key = @key AND permission = @permission AND status = 'granted' FOR UPDATE`

// Reinstate lifts the revocation of permission, granting it scope, in every
// staged set of key whose version requests at least scope: an
// administrator's grant, the counterpart of Revoke. A set whose version
// requests less stays revoked, so that version still runs without it.
func Reinstate(ctx context.Context, tx *gorm.DB, key, permission string, scope map[string]any, by *int64) error {
	requested, err := stagedScopes(ctx, tx, reinstatedSQL, key, permission)
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

// reinstatedSQL locks the staged sets of key that revoke permission and
// reads the scope each one's version requests for it.
const reinstatedSQL = `SELECT s.version, hp->'scope' AS scope
		FROM plugin_staged_grants s
		JOIN plugin_versions v ON v.plugin_key = s.plugin_key AND v.version = s.version
		CROSS JOIN LATERAL jsonb_array_elements(v.manifest->'hostPermissions') hp
		WHERE s.plugin_key = @key AND s.permission = @permission AND s.status = 'revoked' AND hp->>'id' = @permission FOR UPDATE OF s`

// stagedScopes reads version -> scope from sel's (version, scope) rows for
// key and permission (@key, @permission); a NULL scope (a host permission
// requesting none) is nil, a scope that is no JSON object an error.
func stagedScopes(ctx context.Context, tx *gorm.DB, sel, key, permission string) (map[string]map[string]any, error) {
	var rows []struct {
		Version string
		Scope   []byte
	}
	if err := store.G(ctx, tx).Raw(sel, map[string]any{"key": key, "permission": permission}).Scan(&rows).Error; err != nil {
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
// revocation.
func revoked(db *gorm.DB, key string) *gorm.DB {
	return db.Raw(`SELECT permission FROM (
		SELECT DISTINCT ON (permission) permission, status FROM (
			SELECT permission, status, granted_at FROM plugin_permission_grants WHERE plugin_key = @key
			UNION ALL
			SELECT permission, status, granted_at FROM plugin_staged_grants WHERE plugin_key = @key) d
		ORDER BY permission, granted_at DESC, status = 'revoked' DESC) latest
		WHERE status = 'revoked'`, map[string]any{"key": key})
}
