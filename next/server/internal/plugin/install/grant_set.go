package install

import (
	"context"
	"fmt"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/grants"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// SetGrant sets the active version's grant of one host permission (nil
// scope: what the manifest requests). A scope within the current grant
// narrows it (plugin:manage, the route's, is enough); anything else (a
// revoked or denied permission, a wider scope) is a grant decision, checked
// like a consent's: plugin:install, the consent route's, and the grant
// rights by risk (checkGrantRights), never wider than the active manifest
// requests. Staged sets get the same narrowing (grants.Narrow), and a staged
// revocation of the permission is lifted when its version requests at least
// the scope (grants.Reinstate). Audited plugin.grant.narrow /
// plugin.grant.regrant.
func (s *Service) SetGrant(ctx context.Context, key, permission string, scope map[string]any, actorID int64) (*Grant, error) {
	var out Grant
	err := s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		// The plugin row first, as RevokeGrant, consent and the rollout
		// commit lock it before the grant rows (no lock-order deadlock).
		g := store.G(ctx, tx)
		var bumped bumpedTrust
		res := bumpPluginReturningTrust(g, key, &bumped)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return core.ErrNotFound.WithMessage("plugin not found")
		}
		status, hasPub, trust := bumped.Status, bumped.Published, pkg.TrustUnsigned
		if bumped.Trust != nil {
			trust = *bumped.Trust
		}
		if status == StatusAwaitingConsent {
			return core.ErrConflict.WithMessage("the plugin is awaiting consent")
		}
		version, err := CurrentVersion(ctx, tx, key)
		if err != nil {
			return err
		}
		m, err := LoadManifest(ctx, tx, key, version)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(m.HostPermissions, func(hp manifest.HostPermission) bool { return hp.ID == permission })
		if i < 0 {
			return core.ErrNotFound.WithMessage(fmt.Sprintf("permission %q is not requested by version %s", permission, version))
		}
		requested := pkg.NormalizeScope(m.HostPermissions[i].Scope)
		if scope == nil {
			scope = requested
		}
		scope = pkg.NormalizeScope(scope)
		if !pkg.ScopeWithin(scope, requested) {
			return core.InvalidFields(core.FieldError{Field: "scope", Code: "scope_too_wide",
				Message: fmt.Sprintf("scope of %q is wider than requested", permission)}).WithMessage("the scope is wider than requested")
		}
		current, err := LoadGrants(ctx, tx, key)
		if err != nil {
			return err
		}
		prev, held := current[permission]
		by := nullID(actorID)
		d := decision{permission: permission, scope: scope, status: GrantGranted}
		action := "plugin.grant.narrow"
		if held && prev.Status == GrantGranted && pkg.ScopeWithin(scope, prev.Scope) {
			err = grants.Narrow(ctx, tx, key, permission, scope, by)
		} else {
			action = "plugin.grant.regrant"
			if err := s.requireInstall(ctx, actorID); err != nil {
				return err
			}
			if err := s.checkGrantRights(ctx, actorID, []decision{d}, trust, hasPub); err != nil {
				return err
			}
			err = grants.Reinstate(ctx, tx, key, permission, scope, by)
		}
		if err != nil {
			return err
		}
		var hash string
		if err := versionRow(g, key, version).Select("manifest_hash").Take(&hash).Error; err != nil {
			return err
		}
		if err := grants.Record(ctx, tx, key, version, hash, by, false, grantDecisions([]decision{d})); err != nil {
			return err
		}
		if current, err = LoadGrants(ctx, tx, key); err != nil {
			return err
		}
		out = current[permission]
		detail := map[string]any{"permission": permission, "version": version, "scope": scope}
		if held {
			detail["previous"] = map[string]any{"status": prev.Status, "scope": prev.Scope}
		}
		return audit.Audit(ctx, tx, actorID, action, "plugin", key, detail)
	})
	if err != nil {
		return nil, err
	}
	s.Notify(ctx, key)
	return &out, nil
}

// requireInstall fails unless the actor holds plugin:install: granting a
// host permission again is a consent decision, which plugin:manage alone
// must not reach (a revoked or denied low-risk permission needs no grant
// right).
func (s *Service) requireInstall(ctx context.Context, actorID int64) error {
	ok, err := s.d.Authz.Can(ctx, actorID, string(perm.PluginInstall))
	if err != nil || ok {
		return err
	}
	return core.ErrPermissionDenied.WithMessage("granting a host permission again requires plugin:install").
		WithDetails(map[string]any{"permission": string(perm.PluginInstall)})
}

// bumpedTrust is what bumpPluginReturningTrust reads back of the plugin.
type bumpedTrust struct {
	Status    string  `gorm:"column:status;->"`
	Published bool    `gorm:"column:published;->"`
	Trust     *string `gorm:"column:trust;->"` // nil: no publisher row
}

func (bumpedTrust) TableName() string { return model.Plugin{}.TableName() }

// bumpPluginReturningTrust is bumpPlugin reading into dst the plugin's
// status, whether it has a publisher, and its trust level (RowsAffected 0:
// no plugin).
func bumpPluginReturningTrust(db *gorm.DB, key string, dst *bumpedTrust) *gorm.DB {
	return db.Model(dst).Clauses(clause.Returning{Columns: []clause.Column{{Name: "status"},
		{Name: "publisher_id IS NOT NULL AS published", Raw: true},
		{Name: "(SELECT trust_level FROM publishers WHERE id = plugins.publisher_id) AS trust", Raw: true}}}).
		Where("key = ?", key).Updates(map[string]any{"row_version": gorm.Expr("row_version + 1"), "updated_at": gorm.Expr("now()")})
}
