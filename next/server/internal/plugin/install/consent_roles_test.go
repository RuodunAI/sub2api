package install

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
)

// Handing a version's new permissions to roles the request names needs
// role:define and the catalog's CanGrantToRoles for every named role, a
// critical permission in a batch of its own that a non-superuser is refused
// (naming the permission); an unknown role is invalid under the field that
// names it, and an upgrade adding no permission checks nothing whatever
// roles it names.
func TestConsentRoleGrantRights(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	upload := func(m *manifest.Manifest) {
		t.Helper()
		_, err := e.svc.Upload(ctx, pkgtest.Build(m, e.root), e.admin, UploadOptions{})
		require.NoError(t, err, m.Version)
	}
	all := manifest.RoleDefault{Unscoped: true}
	exempt := newPerm("rules:exempt", manifest.RiskCritical)
	upload(guardWith("0.1.0", exempt))
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO roles (key, name, builtin) VALUES ('auditor', '{}', false), ('ops', '{}', false) ON CONFLICT (key) DO NOTHING`)
	require.NoError(t, err)
	named := ConsentRequest{Grants: explicitGuardGrants(), NewPermissionRoles: map[string]map[string]manifest.RoleDefault{
		"rules:read": {"auditor": all}, "stats:read": {"auditor": all, "ops": all}}}

	delete(e.authz.perms[e.admin], "role:define")
	_, err = e.svc.Consent(ctx, "guard", "0.1.0", named, e.admin)
	if ce := core.AsError(err); ce.Code != "permission_denied" || ce.Details["permission"] != "role:define" || len(e.perms.checked) != 0 {
		t.Fatalf("named roles without role:define: %v, checked %v", err, e.perms.checked)
	}
	e.authz.perms[e.admin]["role:define"] = true
	e.perms.denyRoles = core.ErrPermissionDenied.WithMessage("you cannot assign or revoke this role").WithDetails(map[string]any{"role": "ops"})
	_, err = e.svc.Consent(ctx, "guard", "0.1.0", named, e.admin)
	assert.Equal(t, "ops", core.AsError(err).Details["role"])
	assert.Equal(t, [][]string{{"auditor", "ops"}}, e.perms.checked)
	e.perms.denyRoles = nil

	// A critical permission named by a non-superuser: the catalog's
	// refusal names the permission.
	e.perms.canGrant = func(_ context.Context, _ pgx.Tx, _ int64, _ []string, critical bool) error {
		if critical {
			return core.ErrPermissionDenied.WithDetails(map[string]any{"reason": "critical", "superuser": true})
		}
		return nil
	}
	critical := ConsentRequest{Grants: explicitGuardGrants(), NewPermissionRoles: map[string]map[string]manifest.RoleDefault{
		"rules:exempt": {"ops": all}, "rules:read": {"auditor": all}}}
	_, err = e.svc.Consent(ctx, "guard", "0.1.0", critical, e.admin)
	ce := core.AsError(err)
	require.Equal(t, "permission_denied", ce.Code)
	assert.Equal(t, "critical", ce.Details["reason"])
	assert.Equal(t, []string{"rules:exempt"}, ce.Details["permissions"])
	assert.Equal(t, []bool{false, false, true}, e.perms.critical)
	e.perms.canGrant = nil

	// An unknown role is invalid under the field that names it, also for a
	// permission the catalog has (P4F4).
	unknown := ConsentRequest{Grants: explicitGuardGrants(), NewPermissionRoles: map[string]map[string]manifest.RoleDefault{"rules:read": {"nope": all}}}
	_, err = e.svc.Consent(ctx, "guard", "0.1.0", unknown, e.admin)
	assert.Equal(t, "unknown", fieldCode(err, "new_permission_roles.rules:read"))

	_, err = e.svc.Consent(ctx, "guard", "0.1.0", named, e.admin)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"auditor": {""}, "ops": {""}}, lastDef(t, e, "plugin.guard:stats:read").GrantRoles)

	// 0.1.1 declares the same permissions, which the catalog now has.
	addPerms(t, e, map[string]string{"rules:read": "low", "rules:manage": "low", "stats:read": "low", "rules:exempt": "critical"})
	upload(guardWith("0.1.1", exempt))
	checked := len(e.perms.checked)
	_, err = e.svc.Consent(ctx, "guard", "0.1.1", critical, e.admin)
	require.NoError(t, err, "an upgrade adding no permission")
	assert.Len(t, e.perms.checked, checked)
}

// A permission inheriting from an old key the catalog has is a rename: the
// consent hands it no defaults, only the old key's holders, every row and,
// when the old key ranges over the same resource, scoped ones; without the
// old key (a first install) it gets its defaults. The consent page shows
// the same.
func TestConsentRenameInheritsOnly(t *testing.T) {
	for _, old := range []string{"", "plugin:flow", "plugin:ticket"} {
		e := newEnv(t)
		ctx := context.Background()
		m := guardWith("0.1.0")
		m.UserPermissions[0].Scope = &manifest.PermissionScope{Resource: "flow", Own: true}
		m.UserPermissions[0].Defaults = map[string]manifest.RoleDefault{"user": {Scopes: []string{"own"}}}
		m.UserPermissions[0].InheritFrom = []string{"rules:view"}
		m.UserPermissions[1].Defaults = map[string]manifest.RoleDefault{"admin": {Unscoped: true}}
		_, err := e.svc.Upload(ctx, pkgtest.Build(m, e.root), e.admin, UploadOptions{})
		require.NoError(t, err)
		want := map[string][]string{"user": {"own"}}
		if old != "" {
			_, err := e.db.Pool.Exec(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, scope_type)
VALUES ('plugin.guard:rules:view', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', $1)`, old)
			require.NoError(t, err)
			_, err = e.db.Pool.Exec(ctx, `
INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = 'operator' AND p.key = 'plugin.guard:rules:view';
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, 'flow/3' FROM roles r, permissions p WHERE r.key = 'supplier' AND p.key = 'plugin.guard:rules:view'`)
			require.NoError(t, err)
			want = map[string][]string{"operator": {""}}
			if old == "plugin:flow" {
				want["supplier"] = []string{"flow/3"}
			}
		}
		r, err := e.svc.Review(ctx, "guard", "0.1.0", e.admin)
		require.NoError(t, err)
		read := r.UserPermissions[0]
		assert.Equal(t, old != "", read.Renamed, old)
		for _, role := range []string{"operator", "supplier", "user"} {
			assert.Equal(t, append([]string{}, want[role]...), read.Cells[role].Grant, "%s %s", old, role)
		}
		_, err = e.svc.Consent(ctx, "guard", "0.1.0", ConsentRequest{Grants: explicitGuardGrants()}, e.admin)
		require.NoError(t, err)
		assert.Equal(t, want, lastDef(t, e, "plugin.guard:rules:read").GrantRoles, old)
		assert.Equal(t, map[string][]string{"admin": {""}}, lastDef(t, e, "plugin.guard:rules:manage").GrantRoles, old)
	}
}

// Inheritance follows the holders before the consent only (a key created
// by the same consent is no source), keeps scoped grants of the same
// scope_type only, and a critical permission inherits from critical keys
// only; every source left out is audited (inherit_skipped). Critical to
// critical inherits, withheld when the consenter is no superuser.
func TestConsentInheritRules(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	_, err := e.db.Pool.Exec(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, risk, scope_type)
VALUES ('plugin.guard:old:exempt', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'critical', ''),
       ('plugin.guard:old:low', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', ''),
       ('plugin.guard:old:flow', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:ticket');
INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p
 WHERE r.key = 'auditor' AND p.key IN ('plugin.guard:old:exempt', 'plugin.guard:old:low', 'plugin.guard:old:flow');
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, 'ticket/9' FROM roles r, permissions p
 WHERE r.key = 'operator' AND p.key = 'plugin.guard:old:flow'`)
	require.NoError(t, err)
	exempt := newPerm("rules:exempt", manifest.RiskCritical)
	exempt.InheritFrom = []string{"old:exempt"}
	strict := newPerm("rules:strict", manifest.RiskCritical)
	strict.InheritFrom = []string{"old:low"} // low: skipped
	flow := newPerm("flow:edit", "")
	flow.Scope = &manifest.PermissionScope{Resource: "flow", Own: true}
	flow.InheritFrom = []string{"old:flow"} // ticket scopes: skipped
	chained := newPerm("rules:chained", "")
	chained.InheritFrom = []string{"rules:exempt"} // created by this consent: no source
	_, err = e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", exempt, strict, flow, chained), e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	res, err := e.svc.Consent(ctx, "guard", "0.1.1", ConsentRequest{Grants: explicitGuardGrants()}, e.admin)
	require.NoError(t, err)
	// The consenter is no superuser: the critical inherited grant waits.
	require.Len(t, res.Withheld, 1)
	assert.Equal(t, RoleGrant{Permission: "plugin.guard:rules:exempt", Role: "auditor", Risk: manifest.RiskCritical, Via: viaInherit, NeedsConsent: true, Withheld: true}, res.Withheld[0])
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:exempt").GrantRoles)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:strict").GrantRoles)
	assert.Equal(t, map[string][]string{"auditor": {""}}, lastDef(t, e, "plugin.guard:flow:edit").GrantRoles)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:chained").GrantRoles)
	var skipped string
	require.NoError(t, e.db.Pool.QueryRow(ctx, `SELECT detail->>'inherit_skipped' FROM audit_logs WHERE action = $1 AND detail->>'version' = '0.1.1'`, AuditConsent).Scan(&skipped))
	assert.JSONEq(t, `[{"permission":"plugin.guard:rules:strict","from":"plugin.guard:old:low","reason":"risk"},
		{"permission":"plugin.guard:flow:edit","from":"plugin.guard:old:flow","reason":"scope_type"}]`, skipped)
}

// realCatalog puts the authz catalog behind e's install service
// (permissions, defaults, grantees) and makes e.admin a super admin, so
// that syncs and grants run the catalog's own checks.
func realCatalog(t *testing.T, e *env, ctx context.Context) {
	t.Helper()
	cat := authz.New(authz.Deps{DB: e.db})
	require.NoError(t, cat.Start(ctx))
	require.NoError(t, e.db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := cat.SetUserRoles(ctx, tx, 0, e.admin, []string{authz.RoleSuperAdmin})
		return err
	}))
	e.svc.d.Permissions, e.svc.d.Defaults = cat, NewDefaultsApplier(cat)
}

// scopeGrants lists role's scope grants of the guard permission local,
// sorted.
func scopeGrants(t *testing.T, e *env, ctx context.Context, role, local string) []string {
	t.Helper()
	var out []string
	require.NoError(t, e.db.Pool.QueryRow(ctx, `SELECT COALESCE(array_agg(rs.scope ORDER BY rs.scope), '{}') FROM role_permission_scopes rs
JOIN roles r ON r.id = rs.role_id JOIN permissions p ON p.id = rs.permission_id
WHERE r.key = $1 AND p.key = $2`, role, "plugin.guard:"+local).Scan(&out))
	return out
}

// Against the real catalog, an inherited scoped grant the new key's scope
// does not take (own no longer allowed, a value no longer declared) is left
// out (inherit_skipped "scope") instead of failing the consent; the scoped
// grants it takes are inherited.
func TestConsentInheritsOnlyScopesTheNewKeyTakes(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	_, err := e.db.Pool.Exec(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, risk, scope_type, scope)
VALUES ('plugin.guard:old:flow', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:flow', '{"resource":"flow","own":true}'),
       ('plugin.guard:old:kind', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:value',
        '{"resource":"value","values":[{"key":"a"},{"key":"b"}]}');
INSERT INTO role_permission_scopes (role_id, permission_id, scope)
SELECT r.id, p.id, v.scope FROM roles r, permissions p,
    (VALUES ('plugin.guard:old:flow', 'own'), ('plugin.guard:old:flow', 'flow/3'),
            ('plugin.guard:old:kind', 'value/a'), ('plugin.guard:old:kind', 'value/b')) v(key, scope)
WHERE r.key = 'auditor' AND p.key = v.key`)
	require.NoError(t, err)
	flow := newPerm("flow:edit", "")
	flow.Scope = &manifest.PermissionScope{Resource: "flow", Options: "/rules"} // own dropped
	flow.InheritFrom = []string{"old:flow"}
	kind := newPerm("kind:edit", "")
	kind.Scope = &manifest.PermissionScope{Values: []manifest.ScopeValue{{Key: "a", Label: manifest.LocalizedText{"en": "A"}}}} // b deleted
	kind.InheritFrom = []string{"old:kind"}
	_, err = e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", flow, kind), e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	res, err := e.svc.Consent(ctx, "guard", "0.1.1", ConsentRequest{Grants: explicitGuardGrants()}, e.admin)
	require.NoError(t, err)
	assert.Empty(t, res.Withheld)
	assert.Equal(t, []string{"flow/3"}, scopeGrants(t, e, ctx, "auditor", "flow:edit"))
	assert.Equal(t, []string{"value/a"}, scopeGrants(t, e, ctx, "auditor", "kind:edit"))
	var skipped string
	require.NoError(t, e.db.Pool.QueryRow(ctx, `SELECT detail->>'inherit_skipped' FROM audit_logs WHERE action = $1 AND detail->>'version' = '0.1.1'`, AuditConsent).Scan(&skipped))
	assert.JSONEq(t, `[{"permission":"plugin.guard:flow:edit","from":"plugin.guard:old:flow","reason":"scope"},
		{"permission":"plugin.guard:kind:edit","from":"plugin.guard:old:kind","reason":"scope"}]`, skipped)
}

// A staged upgrade (another version runs) that narrows a scope keeps the
// running version's definition and scope grants at consent: neither a
// failed activation nor a later version taking the scope again loses them.
// The activation (the runtime's ApplyDefaults) switches to the activated
// version's scope, pruning only then.
func TestStagedUpgradePrunesScopesAtActivation(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	flowAt := func(version string, own bool) *manifest.Manifest {
		flow := newPerm("flow:manage", "")
		flow.Scope = &manifest.PermissionScope{Resource: "flow", Options: "/rules", Own: own}
		return guardWith(version, flow)
	}
	upload := func(m *manifest.Manifest) {
		t.Helper()
		r, err := e.svc.Upload(ctx, pkgtest.Build(m, e.root), e.admin, UploadOptions{})
		require.NoError(t, err)
		require.Equal(t, ConsentApproved, r.ConsentStatus, "carried over")
	}
	activate := func(m *manifest.Manifest) {
		t.Helper()
		require.NoError(t, e.db.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE plugins SET status = 'enabled', active_version = $1 WHERE key = 'guard'`, m.Version); err != nil {
				return err
			}
			return e.svc.d.Defaults.ApplyDefaults(ctx, tx, m, nil)
		}))
	}
	declaresOwn := func() bool {
		t.Helper()
		var own bool
		require.NoError(t, e.db.Pool.QueryRow(ctx, `SELECT COALESCE((scope->>'own')::boolean, false) FROM permissions WHERE key = 'plugin.guard:flow:manage'`).Scan(&own))
		return own
	}
	v1, v2, v3 := flowAt("0.1.1", true), flowAt("0.1.2", false), flowAt("0.1.3", true)
	upload(v1)
	activate(v1)
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO role_permission_scopes (role_id, permission_id, scope)
SELECT r.id, p.id, 'own' FROM roles r, permissions p WHERE r.key = 'operator' AND p.key = 'plugin.guard:flow:manage'`)
	require.NoError(t, err)

	upload(v2)
	assert.Equal(t, []string{"own"}, scopeGrants(t, e, ctx, "operator", "flow:manage"), "v1 still serves own")
	assert.True(t, declaresOwn(), "the catalog keeps v1's scope")
	upload(v3)
	assert.Equal(t, []string{"own"}, scopeGrants(t, e, ctx, "operator", "flow:manage"))
	activate(v3)
	assert.Equal(t, []string{"own"}, scopeGrants(t, e, ctx, "operator", "flow:manage"), "v3 takes own")

	activate(v2)
	assert.Empty(t, scopeGrants(t, e, ctx, "operator", "flow:manage"), "pruned at v2's activation")
	assert.False(t, declaresOwn())
}
