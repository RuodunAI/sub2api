package install

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// roleGrantsEnv installs guard 0.1.0 with every host permission and gives the catalog its three
// permissions (the fake SyncPlugin writes none), rules:manage and the
// removed rules:old held by the custom role auditor; the built-in roles
// exist.
func roleGrantsEnv(t *testing.T) (*env, context.Context) {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	m := pkgtest.Guard("guard", "0.1.0", "sub2api")
	_, err := e.svc.Upload(ctx, pkgtest.Build(m, e.root), e.admin, UploadOptions{})
	require.NoError(t, err)
	// Every host permission granted, so that an upgrade requesting the
	// same ones is carried over.
	var all ConsentRequest
	for _, hp := range m.HostPermissions {
		all.Grants = append(all.Grants, GrantInput{Permission: hp.ID})
	}
	_, err = e.svc.Consent(ctx, "guard", "0.1.0", all, e.admin)
	require.NoError(t, err)
	_, err = e.db.SQL.ExecContext(ctx, `
INSERT INTO roles (key, name, builtin) VALUES ('admin', '{}', true), ('operator', '{}', true), ('supplier', '{}', true), ('user', '{}', true), ('auditor', '{}', false)
ON CONFLICT (key) DO NOTHING;
INSERT INTO permissions (key, module, label, source, plugin_key, status)
SELECT 'plugin.guard:' || k, 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'disabled' FROM unnest(ARRAY['rules:read','rules:manage','stats:read']) k;
INSERT INTO permissions (key, module, label, source, plugin_key, status, risk)
VALUES ('plugin.guard:rules:old', 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'removed', 'low')`)
	require.NoError(t, err)
	testutil.Grant(t, e.db, "auditor", "plugin.guard:rules:manage")
	testutil.Grant(t, e.db, "auditor", "plugin.guard:rules:old")
	return e, ctx
}

// permModel raises m's hostCompat to the first core of the permission
// model, which defaults, risks and scopes need.
func permModel(m *manifest.Manifest) *manifest.Manifest {
	m.HostCompat = ">=" + check.PluginPermissionsFirstCore + " <0.2.0"
	return m
}

// guardWith is guard at version with ups added to its permissions.
func guardWith(version string, ups ...manifest.UserPermission) *manifest.Manifest {
	m := permModel(pkgtest.Guard("guard", version, "sub2api"))
	m.UserPermissions = append(m.UserPermissions, ups...)
	return m
}

// newPerm is a permission of risk ("" low) suggested to roles, every row.
func newPerm(key, risk string, roles ...string) manifest.UserPermission {
	up := manifest.UserPermission{Key: key, Label: manifest.LocalizedText{"en": key}, Risk: risk}
	if len(roles) > 0 {
		up.Defaults = map[string]manifest.RoleDefault{}
	}
	for _, r := range roles {
		up.Defaults[r] = manifest.RoleDefault{Unscoped: true}
	}
	return up
}

// addPerms gives the catalog plugin permissions of guard (the fake
// SyncPlugin writes none), local key -> risk.
func addPerms(t *testing.T, e *env, perms map[string]string) {
	t.Helper()
	for local, risk := range perms {
		_, err := e.db.SQL.ExecContext(context.Background(), `INSERT INTO permissions (key, module, label, source, plugin_key, status, risk)
VALUES ($1, 'plugin.guard', '{"en":"x"}', 'plugin', 'guard', 'active', $2)`, "plugin.guard:"+local, risk)
		require.NoError(t, err)
	}
}

// autoAudits counts the consents of guard nobody gave.
func autoAudits(t *testing.T, e *env, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = 'guard'`, AuditConsentAuto).Scan(&n))
	return n
}

// lastDef is the last synced definition of key.
func lastDef(t *testing.T, e *env, key string) core.PermissionDef {
	t.Helper()
	defs := e.perms.syncs[len(e.perms.syncs)-1].defs
	i := slices.IndexFunc(defs, func(d core.PermissionDef) bool { return d.Key == key })
	require.GreaterOrEqual(t, i, 0, "no def of %s in %+v", key, defs)
	return defs[i]
}

// An upgrade requesting no new host permission is carried over only when
// every role grant of the permissions it creates is of a low risk
// permission to admin, operator or supplier; a sensitive one, one to user
// or one to a custom role (through inheritFrom) leaves it awaiting consent
// with the review saying why. The carried-over one is audited. On the
// consent page an operator's consent goes through whether or not it may
// name the roles: what it may not name is withheld for review (D2), what
// it may is still checked by the catalog's locking CanGrantToRoles.
func TestUpgradeRoleGrantsNeedConsent(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	inherit := newPerm("rules:edit", "", "admin")
	inherit.InheritFrom = []string{"rules:old"} // removed: a rename
	export := "plugin.guard:rules:export"
	grant := func(perm, role, risk, via string, needs bool) RoleGrant {
		return RoleGrant{Permission: perm, Role: role, Risk: risk, Via: via, NeedsConsent: needs, Nameable: true}
	}
	for _, tc := range []struct {
		version string
		up      manifest.UserPermission
		want    string
		grants  []RoleGrant
	}{
		{"0.1.1", newPerm("rules:export", manifest.RiskSensitive, "admin"), ConsentAwaiting,
			[]RoleGrant{grant(export, "admin", manifest.RiskSensitive, viaDefault, true)}},
		{"0.1.2", newPerm("rules:export", "", "admin", "user"), ConsentAwaiting,
			[]RoleGrant{grant(export, "admin", manifest.RiskLow, viaDefault, false), grant(export, "user", manifest.RiskLow, viaDefault, true)}},
		{"0.1.3", inherit, ConsentAwaiting,
			// A rename: no defaults, only the old key's holders.
			[]RoleGrant{grant("plugin.guard:rules:edit", "auditor", manifest.RiskLow, viaInherit, true)}},
		{"0.1.4", newPerm("rules:export", "", "admin", "operator", "supplier"), ConsentApproved,
			[]RoleGrant{grant(export, "admin", manifest.RiskLow, viaDefault, false), grant(export, "operator", manifest.RiskLow, viaDefault, false),
				grant(export, "supplier", manifest.RiskLow, viaDefault, false)}},
	} {
		r, err := e.svc.Upload(ctx, pkgtest.Build(guardWith(tc.version, tc.up), e.root), e.admin, UploadOptions{})
		require.NoError(t, err, tc.version)
		assert.Equal(t, tc.want, r.ConsentStatus, tc.version)
		assert.Equal(t, tc.want == ConsentAwaiting, r.RoleGrantsNeedConsent, tc.version)
		assert.Equal(t, tc.grants, r.NewRoleGrants, tc.version)
		rr, err := e.svc.Review(ctx, "guard", tc.version, e.admin)
		require.NoError(t, err)
		assert.Equal(t, tc.grants, rr.NewRoleGrants, tc.version)
	}
	// Only the carried-over one is audited as an automatic consent, with
	// its grants and nothing pending.
	var detail string
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT detail::text FROM audit_logs WHERE action = $1 AND target_id = 'guard' AND user_id = $2`, AuditConsentAuto, e.admin).Scan(&detail))
	assert.Equal(t, 1, autoAudits(t, e, ctx))
	for _, want := range []string{`"version": "0.1.4"`, `"pending": []`, `"review_required": false`, `"role": "supplier"`} {
		assert.Contains(t, detail, want)
	}
	// Even the carried-over grants go through the catalog's locking check.
	assert.Equal(t, [][]string{{"admin", "operator", "supplier"}}, e.perms.checked)
	e.perms.checked = nil

	// Without role:define nothing is nameable: the consent goes through,
	// both grants withheld and the catalog never asked.
	consent := ConsentRequest{Grants: explicitGuardGrants()}
	e.authz.perms[e.limited]["role:define"] = false
	res, err := e.svc.Consent(ctx, "guard", "0.1.2", consent, e.limited)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "user"}, rolesOf(res.Withheld))
	assert.Empty(t, e.perms.checked)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, export).GrantRoles)

	// A role the catalog says the operator may not name is withheld, the
	// rest granted after the locking check.
	e.perms.grantees = func(context.Context, *gorm.DB, int64, []string) (core.PluginGrantees, error) {
		return core.PluginGrantees{Denied: map[string]string{"admin": "held"}}, nil
	}
	res, err = e.svc.Consent(ctx, "guard", "0.1.1", consent, e.admin)
	require.NoError(t, err)
	require.Len(t, res.Withheld, 1)
	assert.Equal(t, RoleGrant{Permission: export, Role: "admin", Risk: manifest.RiskSensitive, Via: viaDefault, NeedsConsent: true, Withheld: true}, res.Withheld[0])
	e.perms.grantees = nil
	e.perms.denyRoles = core.ErrPermissionDenied.WithMessage("you cannot assign or revoke this role")
	if _, err := e.svc.Consent(ctx, "guard", "0.1.3", consent, e.admin); core.AsError(err).Code != "permission_denied" {
		t.Fatalf("a role the catalog refuses at the locking check: %v", err)
	}
	assert.Equal(t, [][]string{{"auditor"}}, e.perms.checked)
	e.perms.denyRoles = nil
	res, err = e.svc.Consent(ctx, "guard", "0.1.3", consent, e.admin)
	require.NoError(t, err)
	assert.Empty(t, res.Withheld)
	assert.Equal(t, map[string][]string{"auditor": {""}}, lastDef(t, e, "plugin.guard:rules:edit").GrantRoles)
	// The operator consent's audit carries its plan and what it withheld.
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT detail::text FROM audit_logs WHERE action = $1 AND target_id = 'guard'
AND detail->>'version' = '0.1.1'`, AuditConsent).Scan(&detail))
	for _, want := range []string{`"role_grants": [{`, `"pending": [{`, `"withheld": true`, `"inherit_skipped": []`} {
		assert.Contains(t, detail, want)
	}
}

// A built-in plugin's upgrade is approved by the system (node readiness
// needs it), but the grants that need consent are withheld: the rest are
// made, the withheld ones listed for review (with their scope, and the
// catalog's risk) until someone entitled confirms (granting them) or
// rejects them. A built-in first install withholds nothing.
func TestBuiltinUpgradeWithholdsRoleGrants(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	inherit := newPerm("rules:edit", "", "admin")
	inherit.InheritFrom = []string{"rules:old"} // removed: a rename
	dir := t.TempDir()
	m := guardWith("0.1.1", newPerm("rules:export", manifest.RiskSensitive, "admin"), newPerm("rules:view", "", "admin", "user"), inherit)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "guard-0.1.1.s2plugin"), pkgtest.Build(m, e.root), 0o644))
	_ = e.svc.EnsureBuiltin(ctx, dir, testLogger(t)) // the fake rollout cannot upgrade
	var consent string
	require.NoError(t, e.db.SQL.QueryRowContext(ctx, `SELECT consent_status FROM plugin_versions WHERE plugin_key = 'guard' AND version = '0.1.1'`).Scan(&consent))
	require.Equal(t, ConsentApproved, consent)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:export").GrantRoles)
	assert.Equal(t, map[string][]string{"admin": {""}}, lastDef(t, e, "plugin.guard:rules:view").GrantRoles)
	assert.Equal(t, map[string][]string{}, lastDef(t, e, "plugin.guard:rules:edit").GrantRoles)
	assert.Equal(t, 1, autoAudits(t, e, ctx))

	// The catalog now has the new permissions (the real SyncPlugin would
	// have created them).
	addPerms(t, e, map[string]string{"rules:export": manifest.RiskSensitive, "rules:view": manifest.RiskLow, "rules:edit": manifest.RiskLow})
	pending, err := e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
	require.NoError(t, err)
	withheld := func(perm, role, risk, via string) RoleGrant {
		return RoleGrant{Version: "0.1.1", Permission: perm, Role: role, Risk: risk, Via: via, NeedsConsent: true, Withheld: true}
	}
	assert.Equal(t, []RoleGrant{
		withheld("plugin.guard:rules:edit", "auditor", manifest.RiskLow, viaInherit),
		withheld("plugin.guard:rules:export", "admin", manifest.RiskSensitive, viaDefault),
		withheld("plugin.guard:rules:view", "user", manifest.RiskLow, viaDefault),
	}, pending)

	// Confirming needs what naming the roles on the consent page needs.
	if _, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", true, e.limited); core.AsError(err).Details["permission"] != "role:define" {
		t.Fatalf("confirm without role:define: %v", err)
	}
	e.perms.denyRoles = core.ErrPermissionDenied.WithMessage("you cannot assign or revoke this role")
	if _, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", true, e.admin); core.AsError(err).Code != "permission_denied" || len(e.perms.granted) != 0 {
		t.Fatalf("confirm a role the operator may not change: %v, granted %v", err, e.perms.granted)
	}
	assert.Equal(t, []string{"admin", "auditor", "user"}, e.perms.checked[len(e.perms.checked)-1])
	e.perms.denyRoles = nil
	if _, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.0", true, e.admin); core.AsError(err).Code != "not_found" {
		t.Fatalf("a version with nothing to review: %v", err)
	}
	if _, err := e.svc.ReviewRoleGrants(ctx, "absent", "0.1.1", true, e.admin); core.AsError(err).Message != "plugin not found" {
		t.Fatalf("a plugin that does not exist: %v", err)
	}
	_, err = e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", true, e.admin)
	require.NoError(t, err)
	assert.Equal(t, []string{"plugin.guard:rules:edit=auditor", "plugin.guard:rules:export=admin", "plugin.guard:rules:view=user"}, e.perms.granted)
	pending, err = e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
	require.NoError(t, err)
	assert.Empty(t, pending)

	// The next built-in upgrade withholds again; rejecting grants nothing
	// and clears the list.
	m = guardWith("0.1.2", newPerm("rules:export", manifest.RiskSensitive, "admin"), newPerm("rules:view", "", "admin", "user"), inherit, newPerm("stats:export", "", "user"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "guard-0.1.2.s2plugin"), pkgtest.Build(m, e.root), 0o644))
	_ = e.svc.EnsureBuiltin(ctx, dir, testLogger(t))
	addPerms(t, e, map[string]string{"stats:export": manifest.RiskLow})
	pending, err = e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
	require.NoError(t, err)
	if len(pending) != 1 || pending[0].Role != "user" || pending[0].Version != "0.1.2" {
		t.Fatalf("second upgrade pending %+v", pending)
	}
	// Rejecting needs the same rights.
	if _, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.2", false, e.limited); core.AsError(err).Details["permission"] != "role:define" {
		t.Fatalf("reject without role:define: %v", err)
	}
	_, err = e.svc.ReviewRoleGrants(ctx, "guard", "0.1.2", false, e.admin)
	require.NoError(t, err)
	pending, err = e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
	require.NoError(t, err)
	assert.Empty(t, pending)
	assert.Len(t, e.perms.granted, 3)
}

// The review queue lists each (permission, role, scope) a consent withheld
// until the role holds it: a scoped row hides that scope only, a row of
// every row hides them all; a scope the permission no longer declares and a
// removed permission are not listed.
func TestPendingRoleGrantsScoped(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	dir := t.TempDir()
	check := newPerm("input:check", "")
	check.Scope = &manifest.PermissionScope{Values: []manifest.ScopeValue{{Key: "input", Label: manifest.LocalizedText{"en": "Input"}},
		{Key: "output", Label: manifest.LocalizedText{"en": "Output"}}}}
	check.Defaults = map[string]manifest.RoleDefault{"user": {Scopes: []string{"input", "output"}}}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "guard-0.1.1.s2plugin"), pkgtest.Build(guardWith("0.1.1", check), e.root), 0o644))
	_ = e.svc.EnsureBuiltin(ctx, dir, testLogger(t))
	addPerms(t, e, map[string]string{"input:check": manifest.RiskLow})
	declare := func(values string) {
		t.Helper()
		_, err := e.db.SQL.ExecContext(ctx, `UPDATE permissions SET scope_type = 'plugin:value', scope = jsonb_build_object('resource', 'value', 'values', $1::jsonb)
WHERE key = 'plugin.guard:input:check'`, values)
		require.NoError(t, err)
	}
	declare(`[{"key":"input"},{"key":"output"}]`)
	scopes := func() []string {
		t.Helper()
		gs, err := e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
		require.NoError(t, err)
		out := []string{}
		for _, g := range gs {
			out = append(out, g.Role+":"+g.Scope)
		}
		return out
	}
	assert.Equal(t, []string{"user:value/input", "user:value/output"}, scopes())
	grant := func(sql string) {
		t.Helper()
		_, err := e.db.SQL.ExecContext(ctx, sql)
		require.NoError(t, err)
	}
	testutil.Grant(t, e.db, "user", "plugin.guard:input:check", "value/input")
	assert.Equal(t, []string{"user:value/output"}, scopes())
	testutil.Grant(t, e.db, "user", "plugin.guard:input:check")
	assert.Empty(t, scopes())
	testutil.Revoke(t, e.db, "user", "plugin.guard:input:check")
	assert.Len(t, scopes(), 2)
	declare(`[{"key":"input"}]`)
	assert.Equal(t, []string{"user:value/input"}, scopes())
	grant(`UPDATE permissions SET status = 'removed' WHERE key = 'plugin.guard:input:check'`)
	assert.Empty(t, scopes())

	// A pending grant naming another plugin's permission is never listed,
	// so a review of guard cannot grant it (security #10).
	grant(`INSERT INTO plugins (key, name, status) VALUES ('other', '{}', 'enabled');
INSERT INTO permissions (key, module, label, source, plugin_key, status) VALUES ('plugin.other:x:read', 'plugin.other', '{}', 'plugin', 'other', 'active');
INSERT INTO audit_logs (action, target_type, target_id, detail) VALUES ('plugin.consent', 'plugin', 'guard',
  '{"version":"0.1.9","pending":[{"permission":"plugin.other:x:read","role":"user","scope":""}]}')`)
	assert.Empty(t, scopes())
}

// Two built-in upgrades withholding the same grant list it once (under the
// earlier version); rejecting it there rejects it for both, while a grant
// only the later one withheld stays listed. A later upgrade withholding it
// again after the reject lists it again.
func TestRejectWithheldGrantAcrossVersions(t *testing.T) {
	e, ctx := roleGrantsEnv(t)
	dir := t.TempDir()
	upgrade := func(version string, ups ...manifest.UserPermission) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "guard-"+version+".s2plugin"), pkgtest.Build(guardWith(version, ups...), e.root), 0o644))
		_ = e.svc.EnsureBuiltin(ctx, dir, testLogger(t)) // the fake rollout cannot upgrade
	}
	pending := func() []RoleGrant {
		t.Helper()
		gs, err := e.svc.PendingRoleGrants(ctx, e.db.Gorm, "guard", "")
		require.NoError(t, err)
		return gs
	}
	upgrade("0.1.1", newPerm("rules:view", "", "admin", "user"))
	upgrade("0.1.2", newPerm("rules:view", "", "admin", "user"), newPerm("stats:export", "", "user"))
	addPerms(t, e, map[string]string{"rules:view": manifest.RiskLow, "stats:export": manifest.RiskLow})
	want := []RoleGrant{
		{Version: "0.1.1", Permission: "plugin.guard:rules:view", Role: "user", Risk: manifest.RiskLow, Via: viaDefault, NeedsConsent: true, Withheld: true},
		{Version: "0.1.2", Permission: "plugin.guard:stats:export", Role: "user", Risk: manifest.RiskLow, Via: viaDefault, NeedsConsent: true, Withheld: true},
	}
	assert.Equal(t, want, pending())
	_, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", false, e.admin)
	require.NoError(t, err)
	assert.Equal(t, want[1:], pending())
	// The 0.1.2 reject audits only what was still pending under it.
	gs, err := e.svc.ReviewRoleGrants(ctx, "guard", "0.1.2", false, e.admin)
	require.NoError(t, err)
	if len(gs) != 1 || gs[0].Permission != "plugin.guard:stats:export" {
		t.Fatalf("rejecting 0.1.2: %+v", gs)
	}
	assert.Empty(t, pending())
	assert.Empty(t, e.perms.granted)
	// rules:view new again (gone from the catalog meanwhile): 0.1.3
	// withholds it after the reject, so it is listed again.
	require.NoError(t, e.db.Tx(ctx, func(tx *gorm.DB) error { return casbinrule.DropPermissions(tx, []string{"plugin.guard:rules:view"}) }))
	_, err = e.db.SQL.ExecContext(ctx, `DELETE FROM permissions WHERE key = 'plugin.guard:rules:view'`)
	require.NoError(t, err)
	upgrade("0.1.3", newPerm("rules:view", "", "admin", "user"), newPerm("stats:export", "", "user"))
	addPerms(t, e, map[string]string{"rules:view": manifest.RiskLow})
	if got := pending(); len(got) != 1 || got[0].Version != "0.1.3" || got[0].Permission != "plugin.guard:rules:view" {
		t.Fatalf("withheld again by 0.1.3: %+v", got)
	}
}
