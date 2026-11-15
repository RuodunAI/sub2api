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

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
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

// Granted returns permission -> scope of what an instance of key@version is
// granted, limited to the host permissions its manifest declares: a grant
// made for another version never reaches it.
func Granted(ctx context.Context, q store.Querier, key, version string, declared []manifest.HostPermission) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(declared) == 0 {
		return out, nil
	}
	ids := make([]string, len(declared))
	for i, hp := range declared {
		ids[i] = hp.ID
	}
	rows, err := q.Query(ctx, `SELECT permission, scope FROM `+VersionRows+`
		WHERE status = 'granted' AND permission = ANY($3)`, key, version, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var perm string
		var scope []byte
		if err := rows.Scan(&perm, &scope); err != nil {
			return nil, err
		}
		if len(scope) == 0 {
			scope = []byte("{}")
		}
		out[perm] = scope
	}
	return out, rows.Err()
}

// Holds reports whether key@version is granted permission now, with exactly
// scope unless it is nil: the per-call check behind an instance's cached
// grants, and the rollout's check of db.schema before migrating.
func Holds(ctx context.Context, q store.Querier, key, version, permission string, scope []byte) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+VersionRows+`
		WHERE permission = $3 AND status = 'granted' AND ($4::jsonb IS NULL OR scope = $4::jsonb))`, key, version, permission, scope).Scan(&ok)
	return ok, err
}

// Decision is one host permission decision of a consent.
type Decision struct {
	Permission string
	Scope      []byte
	Status     string
}

// Record writes the decisions of a consent to key@version: staged while
// another version runs, else as the plugin's grants.
func Record(ctx context.Context, tx pgx.Tx, key, version, manifestHash string, by *int64, staged bool, ds []Decision) error {
	sql := `INSERT INTO plugin_permission_grants (plugin_key, plugin_version, manifest_hash, granted_by, permission, scope, status, granted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (plugin_key, permission) DO UPDATE SET
		  scope = EXCLUDED.scope, status = EXCLUDED.status, plugin_version = EXCLUDED.plugin_version,
		  manifest_hash = EXCLUDED.manifest_hash, granted_by = EXCLUDED.granted_by, granted_at = now()`
	if staged {
		sql = `INSERT INTO plugin_staged_grants (plugin_key, version, manifest_hash, granted_by, permission, scope, status, granted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now())
			ON CONFLICT (plugin_key, version, permission) DO UPDATE SET
			  scope = EXCLUDED.scope, status = EXCLUDED.status, manifest_hash = EXCLUDED.manifest_hash,
			  granted_by = EXCLUDED.granted_by, granted_at = now()`
	}
	for _, d := range ds {
		if _, err := tx.Exec(ctx, sql, key, version, manifestHash, by, d.Permission, d.Scope, d.Status); err != nil {
			return err
		}
	}
	return nil
}

// Promote makes the set staged for version the plugin's grants; the rollout
// commit calls it when version becomes active (nothing staged: no-op).
func Promote(ctx context.Context, tx pgx.Tx, key, version string) error {
	_, err := tx.Exec(ctx, `
		WITH staged AS (DELETE FROM plugin_staged_grants WHERE plugin_key = $1 AND version = $2 RETURNING *)
		INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash, granted_by, granted_at)
		SELECT plugin_key, permission, scope, status, version, manifest_hash, granted_by, granted_at FROM staged
		ON CONFLICT (plugin_key, permission) DO UPDATE SET
		  scope = EXCLUDED.scope, status = EXCLUDED.status, plugin_version = EXCLUDED.plugin_version,
		  manifest_hash = EXCLUDED.manifest_hash, granted_by = EXCLUDED.granted_by, granted_at = EXCLUDED.granted_at`, key, version)
	return err
}

// Withdraw takes back the set staged for version when its upgrade is
// abandoned (reason: the rollout's final phase): the version waits for a
// new consent, and the system audits plugin.grants.withdraw with the
// permissions that were granted. A revocation in the set is kept as a
// plugin grant row unless the plugin has one for that permission (it was
// revoked with it, or decided since), so it still stands (Revoked).
// Nothing staged: no-op.
func Withdraw(ctx context.Context, tx pgx.Tx, key, version, reason string) error {
	var (
		n       int
		granted []string
	)
	if err := tx.QueryRow(ctx, `
		WITH gone AS (DELETE FROM plugin_staged_grants WHERE plugin_key = $1 AND version = $2 RETURNING *),
		kept AS (INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash, granted_by, granted_at)
			SELECT plugin_key, permission, scope, status, version, manifest_hash, granted_by, granted_at FROM gone WHERE status = 'revoked'
			ON CONFLICT (plugin_key, permission) DO NOTHING)
		SELECT count(*), COALESCE(array_agg(permission ORDER BY permission) FILTER (WHERE status = 'granted'), '{}') FROM gone`,
		key, version).Scan(&n, &granted); err != nil || n == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE plugin_versions SET consent_status = 'awaiting_consent'
		WHERE plugin_key = $1 AND version = $2 AND consent_status = 'approved'`, key, version); err != nil {
		return err
	}
	return audit.Audit(ctx, tx, 0, "plugin.grants.withdraw", "plugin", key, map[string]any{
		"version": version, "reason": reason, "permissions": granted,
	})
}

// Revoke marks permission revoked wherever key grants it: the plugin's
// grants and every staged set (an emergency revocation must not come back
// with the next activation). It reports whether a grant was revoked.
func Revoke(ctx context.Context, tx pgx.Tx, key, permission string, by *int64) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `
		WITH active AS (UPDATE plugin_permission_grants SET status = 'revoked', granted_by = $3, granted_at = now()
			WHERE plugin_key = $1 AND permission = $2 AND status = 'granted' RETURNING 1),
		staged AS (UPDATE plugin_staged_grants SET status = 'revoked', granted_by = $3, granted_at = now()
			WHERE plugin_key = $1 AND permission = $2 AND status = 'granted' RETURNING 1)
		SELECT (SELECT count(*) FROM active) + (SELECT count(*) FROM staged)`, key, permission, by).Scan(&n)
	return n > 0, err
}

// Narrow narrows permission to scope in every staged set of key that grants
// it a wider scope, as an administrator narrows the plugin's grant (the
// caller records that one): activating the version must not widen it again.
// A set granting a scope neither wider nor within scope keeps it: it is that
// version's own decision, on another request.
func Narrow(ctx context.Context, tx pgx.Tx, key, permission string, scope map[string]any, by *int64) error {
	granted, err := stagedScopes(ctx, tx, `SELECT version, scope FROM plugin_staged_grants
		WHERE plugin_key = $1 AND permission = $2 AND status = 'granted' FOR UPDATE`, key, permission)
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
func Reinstate(ctx context.Context, tx pgx.Tx, key, permission string, scope map[string]any, by *int64) error {
	requested, err := stagedScopes(ctx, tx, `SELECT s.version, hp->'scope'
		FROM plugin_staged_grants s
		JOIN plugin_versions v ON v.plugin_key = s.plugin_key AND v.version = s.version
		CROSS JOIN LATERAL jsonb_array_elements(v.manifest->'hostPermissions') hp
		WHERE s.plugin_key = $1 AND s.permission = $2 AND s.status = 'revoked' AND hp->>'id' = $2 FOR UPDATE OF s`, key, permission)
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

// stagedScopes reads version -> scope from sel's (version, scope) rows for
// key and permission ($1, $2); a NULL scope (a host permission requesting
// none) is nil, a scope that is no JSON object an error.
func stagedScopes(ctx context.Context, tx pgx.Tx, sel, key, permission string) (map[string]map[string]any, error) {
	rows, err := tx.Query(ctx, sel, key, permission)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var (
			version string
			raw     []byte
			s       map[string]any
		)
		if err := rows.Scan(&version, &raw); err != nil {
			return nil, err
		}
		if raw != nil {
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("scope of %s@%s %s: %w", key, version, permission, err)
			}
		}
		out[version] = s
	}
	return out, rows.Err()
}

// grantVersions grants permission with scope in the staged sets of key for
// versions.
func grantVersions(ctx context.Context, tx pgx.Tx, key, permission string, scope map[string]any, by *int64, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	b, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE plugin_staged_grants SET status = 'granted', scope = $4, granted_by = $5, granted_at = now()
		WHERE plugin_key = $1 AND permission = $2 AND version = ANY($3)`, key, permission, versions, b, by)
	return err
}

// Revoked reports the host permissions of key whose latest decision, over
// the plugin's grants and every staged set, is a revocation: an
// administrator revoked them and no consent has granted them since. A
// consent grants them only when it names them (CONTRACTS §5.7).
func Revoked(ctx context.Context, q store.Querier, key string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT permission FROM (
		SELECT DISTINCT ON (permission) permission, status FROM (
			SELECT permission, status, granted_at FROM plugin_permission_grants WHERE plugin_key = $1
			UNION ALL
			SELECT permission, status, granted_at FROM plugin_staged_grants WHERE plugin_key = $1) d
		ORDER BY permission, granted_at DESC, status = 'revoked' DESC) latest
		WHERE status = 'revoked'`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var perm string
		if err := rows.Scan(&perm); err != nil {
			return nil, err
		}
		out[perm] = true
	}
	return out, rows.Err()
}
