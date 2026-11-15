package install

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Role grants of a consent (DECISIONS "插件升级授权" C, D2, D3): the
// permissions a version creates go to the roles of their manifest defaults,
// of the consent's overrides and to the holders of their inheritFrom keys.
// Each grant goes to review instead (withheld) when the consenting actor may
// not name its role (nameable), and on an upgrade nobody consented to when
// it needs consent: an upgrade is approved without anyone only when every
// such grant is of a low risk permission to the built-in admin, operator
// or supplier role (autoGrantRoles); otherwise someone decides on the
// consent page. Built-in plugins upgrade with the core and are approved by
// the system all the same (node readiness needs it), so their grants that
// need consent are withheld and listed for review instead.

// autoGrantRoles are the built-in roles an upgrade may hand low risk new
// permissions without consent.
var autoGrantRoles = []string{"admin", "operator", "supplier"}

// Audit actions of consents and the role grants they withheld.
const (
	AuditConsent           = "plugin.consent"
	AuditConsentAuto       = "plugin.consent.auto"
	AuditRoleGrantsConfirm = "plugin.role_grants.confirm"
	AuditRoleGrantsReject  = "plugin.role_grants.reject"
)

// errRoleGrantsNeedConsent stops an automatic consent whose role grants
// need someone's decision; the version stays awaiting consent.
var errRoleGrantsNeedConsent = errors.New("the version's role grants need consent")

// RoleGrant is one (role, scope) a permission created by a consent goes to.
type RoleGrant struct {
	// Version is the plugin version whose consent withheld the grant (the
	// review list only).
	Version string `json:"version,omitempty"`
	// Permission is the qualified permission key.
	Permission string `json:"permission"`
	Role       string `json:"role"`
	// Scope is the canonical suffix: "" every row (a role_permissions
	// row); "own", "value/input", "flow/3" go to role_permission_scopes.
	Scope string `json:"scope"`
	Risk  string `json:"risk"` // manifest.RiskLow | RiskSensitive | RiskCritical
	// Via is "default" (manifest defaults), "override" (the consent named
	// it) or "inherit" (a holder of an inheritFrom key).
	Via          string `json:"via"`
	NeedsConsent bool   `json:"needs_consent"` // the upgrade rule (autoGrantRoles)
	Nameable     bool   `json:"nameable"`      // the consenting actor may name the role (D2, D3)
	Withheld     bool   `json:"withheld"`      // waits for review instead of being granted
}

const (
	viaDefault  = "default"
	viaOverride = "override"
	viaInherit  = "inherit"
)

// InheritSkip is an inheritFrom source planRoleGrants left out for a
// permission: a scoped grant of a key of another scope_type ("scope_type"),
// or a key that is not critical for a critical permission ("risk").
type InheritSkip struct {
	Permission string `json:"permission"`
	From       string `json:"from"`
	Reason     string `json:"reason"`
}

// planRoleGrants lists the role grants a consent makes for every
// userPermission the catalog does not have yet (existing): its roles
// (roleGrants; via override for the roles overrides names) and the roles
// holding one of its inheritFrom keys now, so only grants made before this
// consent count. Inheritance keeps a scoped grant only when the old key
// ranges over the same resource (scope_type), and a critical permission
// inherits from critical keys only (the manifest check cannot see the risk
// of a key no longer declared); the sources left out are returned. On a
// first install nothing needs consent: someone (or the system) consents
// anyway. Super admin roles hold every permission and are left out.
func planRoleGrants(ctx context.Context, q store.Querier, m *manifest.Manifest, existing map[string]bool,
	roles, overrides map[string]map[string][]string, upgrade bool,
) ([]RoleGrant, []InheritSkip, error) {
	var from []string
	for _, up := range m.UserPermissions {
		for _, k := range up.InheritFrom {
			if key := manifest.PermissionKey(m.Key, k); existing[key] {
				from = append(from, key)
			}
		}
	}
	holders, err := inheritHolders(ctx, q, from)
	if err != nil {
		return nil, nil, err
	}
	var out []RoleGrant
	var skipped []InheritSkip
	add := func(up manifest.UserPermission, role, scope string, builtin bool, via string) {
		g := RoleGrant{Permission: manifest.PermissionKey(m.Key, up.Key), Role: role, Scope: scope, Risk: up.RiskLevel(), Via: via}
		if slices.ContainsFunc(out, func(o RoleGrant) bool { return o.Permission == g.Permission && o.Role == role && o.Scope == scope }) {
			return
		}
		g.NeedsConsent = upgrade && (g.Risk != manifest.RiskLow || !builtin || !slices.Contains(autoGrantRoles, role))
		out = append(out, g)
	}
	skip := func(s InheritSkip) {
		if !slices.Contains(skipped, s) {
			skipped = append(skipped, s)
		}
	}
	for _, up := range m.UserPermissions {
		key := manifest.PermissionKey(m.Key, up.Key)
		if existing[key] {
			continue
		}
		for _, role := range slices.Sorted(maps.Keys(roles[up.Key])) {
			via := viaDefault
			if _, named := overrides[up.Key][role]; named {
				via = viaOverride
			}
			for _, scope := range roles[up.Key][role] {
				// Manifest defaults name built-in roles only (manifest
				// BuiltinRoleKeys). An override may name custom roles:
				// marked built-in here, they need consent all the same,
				// no custom role being one of autoGrantRoles.
				add(up, role, scope, true, via)
			}
		}
		scopeType := ""
		if up.Scope != nil {
			scopeType = core.PluginScopeTypePrefix + up.Scope.ResourceName()
		}
		for _, k := range up.InheritFrom {
			old := manifest.PermissionKey(m.Key, k)
			for _, h := range holders[old] {
				switch {
				case up.RiskLevel() == manifest.RiskCritical && h.risk != manifest.RiskCritical:
					skip(InheritSkip{Permission: key, From: old, Reason: "risk"})
				case h.scope != "" && h.scopeType != scopeType:
					skip(InheritSkip{Permission: key, From: old, Reason: "scope_type"})
				default:
					add(up, h.key, h.scope, h.builtin, viaInherit)
				}
			}
		}
	}
	return out, skipped, nil
}

// roleRef is one grant of a permission: the role, and the grant's scope
// with the permission's scope_type and risk.
type roleRef struct {
	key       string
	builtin   bool
	scope     string // "" every row
	scopeType string
	risk      string
}

// inheritHolders maps each permission key of keys to its grants, every row
// and scoped ones alike, super admin roles aside; removed keys keep theirs
// (D6), so they count too.
func inheritHolders(ctx context.Context, q store.Querier, keys []string) (map[string][]roleRef, error) {
	out := map[string][]roleRef{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
SELECT p.key, r.key, r.builtin, '' AS scope, p.scope_type, p.risk FROM role_permissions rp
  JOIN permissions p ON p.id = rp.permission_id JOIN roles r ON r.id = rp.role_id
 WHERE p.key = ANY($1) AND NOT r.superuser
UNION ALL
SELECT p.key, r.key, r.builtin, rs.scope, p.scope_type, p.risk FROM role_permission_scopes rs
  JOIN permissions p ON p.id = rs.permission_id JOIN roles r ON r.id = rs.role_id
 WHERE p.key = ANY($1) AND NOT r.superuser
ORDER BY 1, 2, 4`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var r roleRef
		if err := rows.Scan(&key, &r.key, &r.builtin, &r.scope, &r.scopeType, &r.risk); err != nil {
			return nil, err
		}
		out[key] = append(out[key], r)
	}
	return out, rows.Err()
}

// needConsent reports whether any grant of gs needs consent.
func needConsent(gs []RoleGrant) bool {
	return slices.ContainsFunc(gs, func(g RoleGrant) bool { return g.NeedsConsent })
}

// rolesOf lists the roles of gs, sorted and deduplicated.
func rolesOf(gs []RoleGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Role)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// withholdGrants splits plan into the grants made now and the ones withheld
// for review (pending), and returns the grants the consent syncs with:
// every permission created now goes to exactly its planned (role, scope)
// pairs not withheld, so that what is granted is what was planned
// (inheritance resolved now cannot pick up a holder added meanwhile).
func withholdGrants(m *manifest.Manifest, existing map[string]bool, plan []RoleGrant) (map[string]map[string][]string, []RoleGrant) {
	roles := map[string]map[string][]string{}
	pending := []RoleGrant{}
	for _, up := range m.UserPermissions {
		if !existing[manifest.PermissionKey(m.Key, up.Key)] {
			roles[up.Key] = map[string][]string{}
		}
	}
	for _, g := range plan {
		if g.Withheld {
			pending = append(pending, g)
			continue
		}
		local, _ := manifest.LocalPermission(m.Key, g.Permission)
		roles[local][g.Role] = append(roles[local][g.Role], g.Scope)
	}
	return roles, pending
}

// auditAutoGrants records the role grants of a consent nobody gave (an
// upgrade carried over, by its uploader; a built-in plugin, by the system)
// and the ones withheld for review.
// Nothing is written when there are none.
func auditAutoGrants(ctx context.Context, tx pgx.Tx, actorID int64, key, version string, st *settlement) error {
	if len(st.plan) == 0 {
		return nil
	}
	granted := []RoleGrant{}
	for _, g := range st.plan {
		if !g.Withheld {
			granted = append(granted, g)
		}
	}
	return audit.Audit(ctx, tx, actorID, AuditConsentAuto, "plugin", key, map[string]any{
		"version": version, "granted": granted, "pending": st.pending, "review_required": len(st.pending) > 0,
		"inherit_skipped": st.skipped,
	})
}

// PendingRoleGrants lists the role grants consents of key withheld for
// review (version "" for every version) since it was installed, that nobody
// confirmed or rejected since and that still mean something: the permission
// exists and is not removed, the role exists and holds neither every row of
// it nor the grant's scope. A confirm or reject clears its version's list;
// a reject also clears the same (permission, role) under every version
// whose consent was recorded before it (earlier or later versions alike),
// since the list shows each grant once (under its earliest version). Risk
// is the catalog's; a grant audited before scopes existed is of every row.
func PendingRoleGrants(ctx context.Context, q store.Querier, key, version string) ([]RoleGrant, error) {
	rows, err := q.Query(ctx, `
SELECT a.detail->>'version', g->>'permission', g->>'role', COALESCE(g->>'scope', ''), p.risk, COALESCE(g->>'via', ''),
       COALESCE((g->>'needs_consent')::boolean, true)
FROM audit_logs a CROSS JOIN LATERAL jsonb_array_elements(a.detail->'pending') g
JOIN permissions p ON p.key = g->>'permission' AND p.status <> 'removed'
WHERE a.action = ANY($3) AND a.target_type = 'plugin' AND a.target_id = $1
  AND jsonb_typeof(a.detail->'pending') = 'array'
  AND a.created_at >= (SELECT installed_at FROM plugins WHERE key = $1)
  AND ($2 = '' OR a.detail->>'version' = $2)
  AND NOT EXISTS (SELECT 1 FROM audit_logs b WHERE b.target_type = 'plugin' AND b.target_id = a.target_id
      AND b.action = ANY($4) AND b.id > a.id
      AND (b.detail->>'version' = a.detail->>'version'
        OR (b.action = $5 AND b.detail->'grants' @> jsonb_build_array(jsonb_build_object('permission', g->>'permission', 'role', g->>'role')))))
  AND EXISTS (SELECT 1 FROM roles r WHERE r.key = g->>'role' AND NOT r.superuser)
  AND NOT EXISTS (SELECT 1 FROM role_permissions rp JOIN roles r ON r.id = rp.role_id
      WHERE r.key = g->>'role' AND rp.permission_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM role_permission_scopes rs JOIN roles r ON r.id = rs.role_id
      WHERE r.key = g->>'role' AND rs.permission_id = p.id AND rs.scope = COALESCE(g->>'scope', ''))
ORDER BY a.id, 2, 3, 4`, key, version, []string{AuditConsentAuto, AuditConsent},
		[]string{AuditRoleGrantsConfirm, AuditRoleGrantsReject}, AuditRoleGrantsReject)
	if err != nil {
		return nil, err
	}
	gs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RoleGrant, error) {
		g := RoleGrant{Withheld: true}
		return g, r.Scan(&g.Version, &g.Permission, &g.Role, &g.Scope, &g.Risk, &g.Via, &g.NeedsConsent)
	})
	if err != nil {
		return nil, err
	}
	type grant struct{ permission, role, scope string }
	seen := map[grant]bool{}
	return slices.DeleteFunc(gs, func(g RoleGrant) bool {
		k := grant{g.Permission, g.Role, g.Scope}
		dup := seen[k]
		seen[k] = true
		return dup
	}), nil
}

// ReviewRoleGrants confirms (grants) or rejects the role grants the consent
// of version withheld for review. Either needs what naming those roles on
// the consent page needs (checkGrantable: role:define and CanGrantToRoles,
// critical ones by a superuser), and clears the version's list, audited;
// rejecting grants nothing and also clears the rejected grants other
// versions withheld (PendingRoleGrants).
func (s *Service) ReviewRoleGrants(ctx context.Context, key, version string, confirm bool, actorID int64) ([]RoleGrant, error) {
	ctx, release, guardErr := core.BeginPluginMutation(ctx, s.d.Mutations)
	if guardErr != nil {
		return nil, guardErr
	}
	defer release()
	var gs []RoleGrant
	err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM plugins WHERE key = $1 FOR UPDATE`, key); err != nil {
			return err
		}
		var err error
		if gs, err = PendingRoleGrants(ctx, tx, key, version); err != nil {
			return err
		}
		if len(gs) == 0 {
			return core.ErrNotFound.WithMessage("no role grants of this version await review")
		}
		if err := s.checkGrantable(ctx, tx, actorID, gs); err != nil {
			return err
		}
		action := AuditRoleGrantsReject
		if confirm {
			action = AuditRoleGrantsConfirm
			if err := s.grantReviewed(ctx, tx, gs); err != nil {
				return err
			}
		}
		return audit.Audit(ctx, tx, actorID, action, "plugin", key, map[string]any{"version": version, "grants": gs})
	})
	if err != nil {
		return nil, err
	}
	if confirm {
		s.Notify(ctx, key)
	}
	return gs, nil
}

// grantReviewed grants gs, which the operator confirmed (checked by the
// caller).
func (s *Service) grantReviewed(ctx context.Context, tx pgx.Tx, gs []RoleGrant) error {
	if s.d.Permissions == nil {
		return core.ErrInternal.WithMessage("the permission catalog is not available")
	}
	byKey := map[string]map[string][]string{}
	for _, g := range gs {
		if byKey[g.Permission] == nil {
			byKey[g.Permission] = map[string][]string{}
		}
		byKey[g.Permission][g.Role] = append(byKey[g.Permission][g.Role], g.Scope)
	}
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		if err := s.d.Permissions.GrantPluginPermission(ctx, tx, k, byKey[k]); err != nil {
			return err
		}
	}
	return nil
}
