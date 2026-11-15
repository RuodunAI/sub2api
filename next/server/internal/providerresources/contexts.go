package providerresources

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func validResourceContext(in core.ResourceContext) bool {
	return validOwner(in.Owner) && validBinding(in.Binding) && bounded(in.PluginKey) && in.Kind == "ptc" && bounded(in.ParentID)
}
func contextArgs(in core.ResourceContext) []any {
	return []any{in.Owner.UserID, in.Owner.GroupID, in.Binding.AccountID, in.Binding.PrincipalID, in.Binding.Generation, in.PluginKey, in.Kind, in.ParentID}
}

const contextPredicate = `user_id=? AND group_id=? AND account_id=? AND principal_id=? AND generation=? AND plugin_key=? AND kind=? AND parent_id=?`

// liveContexts keeps the contexts of owner bound to a ready container that
// is unexpired at at, or is renewID.
func liveContexts(db *gorm.DB, owner core.ResourceOwner, at time.Time, renewID string) *gorm.DB {
	return db.Table("provider_resource_contexts c").Joins("JOIN provider_resources r ON r.public_id=c.resource_id").
		Where(`c.user_id=? AND c.group_id=? AND r.kind='container' AND r.state='ready' AND (r.expires_at>? OR r.public_id=?)`, owner.UserID, owner.GroupID, at, renewID)
}

func (s *Service) contextQuota(ctx context.Context, tx *gorm.DB, owner core.ResourceOwner, extra int, at time.Time, renewID string) error {
	var count int64
	err := liveContexts(store.G(ctx, tx), owner, at, renewID).Count(&count).Error
	if err != nil {
		return err
	}
	if count+int64(extra) > int64(s.limits.MaxContexts) {
		return core.ErrRateLimited.WithMessage("resource context quota exceeded")
	}
	return nil
}

func contextTarget(ctx context.Context, q *gorm.DB, in core.ResourceContext, id string) (core.ProviderResource, error) {
	r, _, err := read(store.G(ctx, q), `user_id=? AND group_id=? AND public_id=?`, in.Owner.UserID, in.Owner.GroupID, id)
	if store.IsNoRows(err) {
		return r, core.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if r.Binding != in.Binding || r.PluginKey != in.PluginKey || r.Kind != "container" || r.State != "ready" || r.ExpiresAt.IsZero() || !r.ExpiresAt.After(time.Now()) {
		return core.ProviderResource{}, core.ErrNotFound
	}
	return r, nil
}

func (s *Service) BindContext(ctx context.Context, in core.ResourceContext) error {
	if !validResourceContext(in) || !bounded(in.ResourceID) {
		return core.ErrInvalidArgument
	}
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockOwner(ctx, tx, in.Owner); err != nil {
			return err
		}
		// Parent IDs belong to the provider identity, not to a tenant namespace.
		// Serialize across owners before checking the existing binding.
		if err := lockRemote(ctx, tx, in.PluginKey, "context:"+in.Kind, in.Binding, in.ParentID); err != nil {
			return err
		}
		if _, err := contextTarget(ctx, tx, in, in.ResourceID); err != nil {
			return err
		}
		g := store.G(ctx, tx)
		var existing model.ProviderResourceContext
		err := g.Select("resource_id", "user_id", "group_id").
			Where(`account_id=? AND principal_id=? AND generation=? AND plugin_key=? AND kind=? AND parent_id=?`, in.Binding.AccountID, in.Binding.PrincipalID, in.Binding.Generation, in.PluginKey, in.Kind, in.ParentID).
			Take(&existing).Error
		if err == nil {
			owner := core.ResourceOwner{UserID: existing.UserID, GroupID: existing.GroupID}
			if existing.ResourceID != in.ResourceID || owner != in.Owner {
				return core.ErrConflict.WithMessage("parent call is already bound to another resource")
			}
			return nil
		}
		if !store.IsNoRows(err) {
			return err
		}
		if err = s.contextQuota(ctx, tx, in.Owner, 1, time.Now(), ""); err != nil {
			return err
		}
		err = g.Create(&model.ProviderResourceContext{
			UserID: in.Owner.UserID, GroupID: in.Owner.GroupID, AccountID: in.Binding.AccountID, PrincipalID: in.Binding.PrincipalID,
			Generation: in.Binding.Generation, PluginKey: in.PluginKey, Kind: in.Kind, ParentID: in.ParentID, ResourceID: in.ResourceID,
		}).Error
		if store.IsUniqueViolation(err, "") {
			return core.ErrConflict
		}
		return err
	})
}
func (s *Service) ResolveContext(ctx context.Context, in core.ResourceContext) (out core.ProviderResource, err error) {
	if !validResourceContext(in) {
		return out, core.ErrInvalidArgument
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockOwner(ctx, tx, in.Owner); err != nil {
			return err
		}
		var bound model.ProviderResourceContext
		err := store.G(ctx, tx).Select("resource_id").Where(contextPredicate, contextArgs(in)...).Take(&bound).Error
		if store.IsNoRows(err) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		out, err = contextTarget(ctx, tx, in, bound.ResourceID)
		return err
	})
	return
}
