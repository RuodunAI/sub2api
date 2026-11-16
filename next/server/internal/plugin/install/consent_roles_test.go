package install

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
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
	_, err := e.db.SQL.ExecContext(ctx, `INSERT INTO roles (key, name, builtin) VALUES ('auditor', '{}', false), ('ops', '{}', false) ON CONFLICT (key) DO NOTHING`)
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
	e.perms.canGrant = func(_ context.Context, _ *gorm.DB, _ int64, _ []string, risk string) error {
		if risk == manifest.RiskCritical {
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
		want := map[string][]string{"supplier_agent": {"own"}, "user": {"own"}}
		if old != "" {
			_, err := e.db.SQL.ExecContext(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, scope_type)
VALUES ('plugin.guard:rules:view', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', $1)`, old)
			require.NoError(t, err)
			testutil.Grant(t, e.db, "operator", "plugin.guard:rules:view")
			testutil.Grant(t, e.db, "supplier", "plugin.guard:rules:view", "flow/3")
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

// Inheritance keeps scoped grants of the same scope_type only (a key the
// manifest still declares is no source: the manifest check refuses it), and a critical permission inherits from critical keys
// only; every source left out is audited (inherit_skipped). Critical to
// critical inherits, withheld when the consenter is no superuser.
func TestConsentInheritRules(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	_, err := e.db.SQL.ExecContext(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, risk, scope_type)
VALUES ('plugin.guard:old:exempt', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'critical', ''),
       ('plugin.guard:old:low', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', ''),
       ('plugin.guard:old:flow', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:ticket')`)
	require.NoError(t, err)
	for _, k := range []string{"old:exempt", "old:low", "old:flow"} {
		testutil.Grant(t, e.db, "auditor", "plugin.guard:"+k)
	}
	testutil.Grant(t, e.db, "operator", "plugin.guard:old:flow", "ticket/9")
	exempt := newPerm("rules:exempt", manifest.RiskCritical)
	exempt.InheritFrom = []string{"old:exempt"}
	strict := newPerm("rules:strict", manifest.RiskCritical)
	strict.InheritFrom = []string{"old:low"} // low: skipped
	flow := newPerm("flow:edit", "")
	flow.Scope = &manifest.PermissionScope{Resource: "flow", Own: true}
	flow.InheritFrom = []string{"old:flow"} // ticket scopes: skipped
	_, err = e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", exempt, strict, flow), e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	res, err := e.svc.Consent(ctx, "guard", "0.1.1", ConsentRequest{Grants: explicitGuardGrants()}, e.admin)
	require.NoError(t, err)
	// The consenter is no superuser: the critical inherited grant waits.
	require.Len(t, res.Withheld, 1)
	assert.Equal(t, RoleGrant{Permission: "plugin.guard:rules:exempt", Role: "auditor", Risk: manifest.RiskCritical, Via: viaInherit, NeedsConsent: true, Withheld: true}, res.Withheld[0])
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:exempt").GrantRoles)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:strict").GrantRoles)
	assert.Equal(t, map[string][]string{"auditor": {""}}, lastDef(t, e, "plugin.guard:flow:edit").GrantRoles)
	var skipped string
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT detail->>'inherit_skipped' FROM audit_logs WHERE action = $1 AND detail->>'version' = '0.1.1'`, AuditConsent).Scan(&skipped))
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
	require.NoError(t, e.db.Tx(ctx, func(tx *gorm.DB) error {
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
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT COALESCE(array_agg(scope ORDER BY scope), '{}') FROM (`+casbinrule.Grants+`) g
WHERE role = $1 AND action = $2 AND scope <> ''`, role, "plugin.guard:"+local).Scan(&out))
	return out
}

// Against the real catalog, an inherited scoped grant the new key's scope
// does not take (own no longer allowed, a value no longer declared) is left
// out (inherit_skipped "scope") instead of failing the consent; the scoped
// grants it takes are inherited.
func TestConsentInheritsOnlyScopesTheNewKeyTakes(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	_, err := e.db.SQL.ExecContext(ctx, `
INSERT INTO permissions (key, module, label, source, plugin_key, status, risk, scope_type, scope)
VALUES ('plugin.guard:old:flow', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:flow', '{"resource":"flow","own":true}'),
       ('plugin.guard:old:kind', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low', 'plugin:value',
        '{"resource":"value","values":[{"key":"a"},{"key":"b"}]}')`)
	require.NoError(t, err)
	testutil.Grant(t, e.db, "auditor", "plugin.guard:old:flow", "own", "flow/3")
	testutil.Grant(t, e.db, "auditor", "plugin.guard:old:kind", "value/a", "value/b")
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
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT detail->>'inherit_skipped' FROM audit_logs WHERE action = $1 AND detail->>'version' = '0.1.1'`, AuditConsent).Scan(&skipped))
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
		require.NoError(t, e.db.Tx(ctx, func(tx *gorm.DB) error {
			if _, err := store.SQLTx(tx).ExecContext(ctx, `UPDATE plugins SET status = 'enabled', active_version = $1 WHERE key = 'guard'`, m.Version); err != nil {
				return err
			}
			return e.svc.d.Defaults.ApplyDefaults(ctx, tx, m, nil)
		}))
	}
	declaresOwn := func() bool {
		t.Helper()
		var own bool
		require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT COALESCE((scope->>'own')::boolean, false) FROM permissions WHERE key = 'plugin.guard:flow:manage'`).Scan(&own))
		return own
	}
	v1, v2, v3 := flowAt("0.1.1", true), flowAt("0.1.2", false), flowAt("0.1.3", true)
	upload(v1)
	activate(v1)
	testutil.Grant(t, e.db, "operator", "plugin.guard:flow:manage", "own")

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

// A consent's override of a renamed permission decides its inheritance
// too (CONTRACTS C2): a holder of the old key the override names gets what
// it names, one it leaves out nothing, and a holder the consenter may not
// name still waits for review (D2).
func TestConsentOverrideDecidesInheritance(t *testing.T) {
	e, ctx := roleGrantsEnv(t) // auditor holds the removed rules:old
	testutil.Grant(t, e.db, "operator", "plugin.guard:rules:old")
	testutil.Grant(t, e.db, "supplier", "plugin.guard:rules:old")
	e.perms.grantees = func(context.Context, *gorm.DB, int64, []string) (core.PluginGrantees, error) {
		return core.PluginGrantees{Denied: map[string]string{"supplier": core.GrantDenyHeld}}, nil
	}
	edit := newPerm("rules:edit", "")
	edit.InheritFrom = []string{"rules:old"}
	_, err := e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", edit), e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	r, err := e.svc.Review(ctx, "guard", "0.1.1", e.admin)
	require.NoError(t, err)
	row := r.UserPermissions[len(r.UserPermissions)-1]
	require.True(t, row.Renamed)
	assert.Equal(t, []string{""}, row.Cells["auditor"].Grant, "the cell's initial value is the inherited grant")

	req := ConsentRequest{Grants: explicitGuardGrants(), NewPermissionRoles: map[string]map[string]manifest.RoleDefault{
		"rules:edit": {"operator": {Unscoped: true}, "auditor": {Scopes: []string{}}}}}
	res, err := e.svc.Consent(ctx, "guard", "0.1.1", req, e.admin)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"operator": {""}}, lastDef(t, e, "plugin.guard:rules:edit").GrantRoles)
	assert.Equal(t, []RoleGrant{{Permission: "plugin.guard:rules:edit", Role: "supplier", Risk: manifest.RiskLow, Via: viaInherit,
		Withheld: true}}, res.Withheld)
}

// A renamed row's cells show no viewer without role:read who holds the old
// key (C4, N4): neither the grant nor withheld, which an unnameable holder
// would set.
func TestRenamedCellsHideOldHolders(t *testing.T) {
	e, ctx := roleGrantsEnv(t) // auditor holds the removed rules:old
	edit := newPerm("rules:edit", "")
	edit.InheritFrom = []string{"rules:old"}
	_, err := e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", edit), e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	delete(e.authz.perms[e.admin], "role:read")
	delete(e.authz.perms[e.admin], "role:define")
	r, err := e.svc.Review(ctx, "guard", "0.1.1", e.admin)
	require.NoError(t, err)
	row := r.UserPermissions[len(r.UserPermissions)-1]
	require.True(t, row.Renamed)
	for role, c := range row.Cells {
		b, err := json.Marshal(c)
		require.NoError(t, err)
		assert.NotContains(t, string(b), `"grant"`, role)
		assert.False(t, c.Withheld, role)
	}
}

// guardRollout drives guard through real consents and activations on the
// real catalog (realCatalog).
type guardRollout struct {
	t   *testing.T
	e   *env
	ctx context.Context
}

// upload uploads m, its consent carried over (approved): staged when a
// version runs.
func (g guardRollout) upload(m *manifest.Manifest) *Review {
	g.t.Helper()
	r, err := g.e.svc.Upload(g.ctx, pkgtest.Build(m, g.e.root), g.e.admin, UploadOptions{})
	require.NoError(g.t, err)
	require.Equal(g.t, ConsentApproved, r.ConsentStatus, "carried over")
	return r
}

// activate makes m the running version, as the rollout's commit does.
func (g guardRollout) activate(m *manifest.Manifest) {
	g.t.Helper()
	require.NoError(g.t, g.e.db.Tx(g.ctx, func(tx *gorm.DB) error {
		if _, err := store.SQLTx(tx).ExecContext(g.ctx, `UPDATE plugins SET status = 'enabled', active_version = $1 WHERE key = 'guard'`, m.Version); err != nil {
			return err
		}
		if err := g.e.svc.d.Defaults.ApplyDefaults(g.ctx, tx, m, nil); err != nil {
			return err
		}
		return g.e.svc.d.Permissions.SetPluginActive(g.ctx, tx, "guard", true)
	}))
}

// setActive disables or enables the running version, as the rollout does.
func (g guardRollout) setActive(active bool) {
	g.t.Helper()
	require.NoError(g.t, g.e.db.Tx(g.ctx, func(tx *gorm.DB) error {
		return g.e.svc.d.Permissions.SetPluginActive(g.ctx, tx, "guard", active)
	}))
}

// state is the catalog status and risk of the guard permission local.
func (g guardRollout) state(local string) [2]string {
	g.t.Helper()
	var status, risk string
	require.NoError(g.t, g.e.db.SQL.QueryRowContext(g.ctx, `SELECT status, risk FROM permissions WHERE key = $1`, "plugin.guard:"+local).Scan(&status, &risk))
	return [2]string{status, risk}
}

// grant gives role every row of the guard permission local.
func (g guardRollout) grant(role, local string) {
	g.t.Helper()
	testutil.Grant(g.t, g.e.db, role, "plugin.guard:"+local)
}

// revoke takes every row of the guard permission local from role.
func (g guardRollout) revoke(role, local string) {
	g.t.Helper()
	testutil.Revoke(g.t, g.e.db, role, "plugin.guard:"+local)
}

// pending is the role grants guard has to review, as viewer sees them.
func (g guardRollout) pending(viewer int64) []RoleGrant {
	g.t.Helper()
	gs, err := g.e.svc.RoleGrantsToReview(g.ctx, g.e.db.Gorm, "guard", viewer)
	require.NoError(g.t, err)
	return gs
}

// A consent made while another version runs is staged (logic #2, #4): a
// key only the new version declares is created staged, a removed key it
// declares again stays removed, and a key both declare keeps the higher
// risk; activation turns them all to the new version's definitions.
func TestStagedConsentWaitsForActivation(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	g := guardRollout{t, e, ctx}
	v1 := guardWith("0.1.1", newPerm("flow:read", manifest.RiskSensitive), newPerm("flow:audit", ""), newPerm("flow:old", ""))
	g.upload(v1)
	g.activate(v1)
	v2 := guardWith("0.1.2", newPerm("flow:read", manifest.RiskSensitive), newPerm("flow:audit", ""))
	g.upload(v2)
	g.activate(v2)
	require.Equal(t, "removed", g.state("flow:old")[0])

	// The running manifest may declare no risk at all (an old format): the
	// new one's risk applies at once when higher, never when lower.
	v3 := guardWith("0.1.3", newPerm("flow:read", ""), newPerm("flow:audit", manifest.RiskSensitive), newPerm("flow:old", ""), newPerm("flow:new", "", "admin"))
	g.upload(v3)
	for local, want := range map[string][2]string{"flow:old": {"removed", "low"}, "flow:new": {"staged", "low"},
		"flow:read": {"active", "sensitive"}, "flow:audit": {"active", "sensitive"}} {
		assert.Equal(t, want, g.state(local), local)
	}
	g.activate(v3)
	for local, want := range map[string][2]string{"flow:old": {"active", "low"}, "flow:new": {"active", "low"},
		"flow:read": {"active", "low"}, "flow:audit": {"active", "sensitive"}} {
		assert.Equal(t, want, g.state(local), local)
	}
	assert.Equal(t, []string{"admin"}, permissionRoles(t, e, ctx, "plugin.guard:flow:new"), "the staged consent's grant stands")
}

// What a staged consent did means nothing until its version activates
// (N3): the running version's removed key keeps its scope grants, a
// staged key stays off when the running version is disabled and enabled
// again, and a later consent discards the staged keys, planning them anew
// for its own version (its defaults, a truthful review).
func TestStagedKeysOfAVersionNeverActivated(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	g := guardRollout{t, e, ctx}
	old := newPerm("flow:old", "")
	old.Scope = &manifest.PermissionScope{Resource: "flow", Own: true}
	read := newPerm("flow:read", "")
	v1 := guardWith("0.1.1", read, old)
	g.upload(v1)
	g.activate(v1)
	testutil.Grant(t, e.db, "operator", "plugin.guard:flow:old", "own")
	v2 := guardWith("0.1.2", read)
	g.upload(v2)
	g.activate(v2)
	require.Equal(t, "removed", g.state("flow:old")[0])

	// v3 declares flow:old again, unscoped, and adds flow:new for admin.
	g.upload(guardWith("0.1.3", read, newPerm("flow:old", ""), newPerm("flow:new", "", "admin")))
	assert.Equal(t, "removed", g.state("flow:old")[0])
	assert.Equal(t, []string{"own"}, scopeGrants(t, e, ctx, "operator", "flow:old"), "pruned at activation, if ever")
	assert.Equal(t, "staged", g.state("flow:new")[0])
	assert.Equal(t, []string{"admin"}, permissionRoles(t, e, ctx, "plugin.guard:flow:new"))

	g.setActive(false)
	g.setActive(true)
	assert.Equal(t, "staged", g.state("flow:new")[0], "v2 does not declare it")

	// v4 supersedes v3, declaring flow:new for operator.
	v4 := guardWith("0.1.4", read, newPerm("flow:new", "", "operator"))
	r := g.upload(v4)
	row := r.UserPermissions[len(r.UserPermissions)-1]
	require.Equal(t, "flow:new", row.Key)
	assert.True(t, row.New)
	assert.Equal(t, []string{""}, row.Cells["operator"].Grant)
	assert.Empty(t, row.Cells["admin"].Grant)
	assert.Equal(t, []string{"operator"}, permissionRoles(t, e, ctx, "plugin.guard:flow:new"), "v3's grant to admin went with its key")
	var discarded string
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT detail::text FROM audit_logs WHERE action = 'authz.sync.discard'`).Scan(&discarded))
	assert.JSONEq(t, `{"keys": ["plugin.guard:flow:new"], "holders": {"plugin.guard:flow:new": ["admin"]}}`, discarded)
	g.activate(v4)
	assert.Equal(t, [2]string{"active", "low"}, g.state("flow:new"))
	assert.Equal(t, []string{"own"}, scopeGrants(t, e, ctx, "operator", "flow:old"))
}

// Holders a renamed key's old key gains between a staged consent and the
// activation retiring it are listed for review then (logic #5), as
// inheriting would have given them; grants the consent planned are not,
// though revoked since (N2), nor anything at a later activation (N1).
// Who holds the old key is role:read's to see (C4, N4).
func TestActivationListsLateHoldersOfRenamedKeys(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	realCatalog(t, e, ctx)
	g := guardRollout{t, e, ctx}
	v1 := guardWith("0.1.1", newPerm("rules:view", ""))
	g.upload(v1)
	g.activate(v1)
	g.grant("operator", "rules:view")

	list := newPerm("rules:list", "")
	list.InheritFrom = []string{"rules:view"}
	v2 := guardWith("0.1.2", list)
	g.upload(v2)
	assert.Equal(t, []string{"operator"}, permissionRoles(t, e, ctx, "plugin.guard:rules:list"), "low to operator: carried over")
	planned, _, err := consentDecisions(ctx, e.db.Gorm, "guard", "0.1.2")
	require.NoError(t, err)
	assert.Equal(t, map[[3]string]bool{{"plugin.guard:rules:list", "operator", ""}: true}, planned, "an automatic consent's plan reads back")

	g.revoke("operator", "rules:list")
	g.grant("supplier", "rules:view")
	g.activate(v2)
	late := RoleGrant{Version: "0.1.2", Permission: "plugin.guard:rules:list", Role: "supplier", Risk: manifest.RiskLow,
		Via: viaInherit, NeedsConsent: true, Withheld: true}
	assert.Equal(t, []RoleGrant{late}, g.pending(e.admin))
	assert.Empty(t, g.pending(e.limited), "no role:read: an inherited grant names a holder of the old key")

	_, err = e.svc.ReviewRoleGrants(ctx, "guard", "0.1.2", false, e.admin)
	require.NoError(t, err)
	v3 := guardWith("0.1.3", list)
	g.upload(v3)
	g.activate(v3)
	assert.Empty(t, g.pending(e.admin), "a later upgrade keeping inheritFrom proposes nothing again")
	assert.Empty(t, permissionRoles(t, e, ctx, "plugin.guard:rules:list"))
}

// permissionRoles lists the roles holding every row of key.
func permissionRoles(t *testing.T, e *env, ctx context.Context, key string) []string {
	t.Helper()
	var out []string
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT COALESCE(array_agg(role ORDER BY role), '{}') FROM (`+casbinrule.Grants+`) g
WHERE action = $1 AND scope = ''`, key).Scan(&out))
	return out
}
