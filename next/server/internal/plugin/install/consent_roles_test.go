package install

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
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
