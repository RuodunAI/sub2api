package install

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
)

// An upgrade whose new permission goes to a built-in role with consent (a
// sensitive one to admin) is approved on the consent page by anyone with
// the grant rights, but the grant is made only for a consenter who may
// change that role (the real catalog: built-in roles are the super
// admin's alone, and a holder never names its own role): a non-super
// admin's consent withholds it for review (D2), which the super admin then
// confirms; the super admin's own consent makes it at once.
func TestUpgradeConsentNeedsRightsOverTheRoles(t *testing.T) {
	for _, superAdmin := range []bool{false, true} {
		e, ctx := roleGrantsEnv(t)
		cat := authz.New(authz.Deps{DB: e.db})
		require.NoError(t, cat.Start(ctx))
		e.perms.canGrant = cat.CanGrantToRoles
		e.perms.grantees = cat.PluginGrantees
		for id, role := range map[int64]string{e.admin: authz.RoleSuperAdmin, e.limited: authz.RoleAdmin} {
			require.NoError(t, e.db.Tx(ctx, func(tx *gorm.DB) error {
				_, err := cat.SetUserRoles(ctx, tx, 0, id, []string{role})
				return err
			}))
			e.authz.perms[id]["role:define"] = true
		}
		r, err := e.svc.Upload(ctx, pkgtest.Build(guardWith("0.1.1", newPerm("rules:export", manifest.RiskSensitive, "admin")), e.root), e.admin, UploadOptions{})
		require.NoError(t, err)
		require.Equal(t, ConsentAwaiting, r.ConsentStatus)
		require.True(t, r.RoleGrantsNeedConsent)
		consenter := e.limited
		if superAdmin {
			consenter = e.admin
		}
		res, err := e.svc.Consent(ctx, "guard", "0.1.1", ConsentRequest{Grants: explicitGuardGrants()}, consenter)
		require.NoError(t, err)
		export := lastDef(t, e, "plugin.guard:rules:export")
		if superAdmin {
			assert.Empty(t, res.Withheld)
			assert.Equal(t, map[string][]string{"admin": {""}}, export.GrantRoles)
			continue
		}
		require.Len(t, res.Withheld, 1)
		assert.Equal(t, "admin", res.Withheld[0].Role)
		assert.Equal(t, map[string][]string{}, export.GrantRoles)
		addPerms(t, e, map[string]string{"rules:export": manifest.RiskSensitive})
		_, err = e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", true, e.limited)
		assert.Error(t, err, "the non-super admin confirms what it may not name")
		_, err = e.svc.ReviewRoleGrants(ctx, "guard", "0.1.1", true, e.admin)
		require.NoError(t, err)
		assert.Equal(t, []string{"plugin.guard:rules:export=admin"}, e.perms.granted)
	}
}
