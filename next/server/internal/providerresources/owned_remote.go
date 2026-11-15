package providerresources

import (
	"context"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// FindOwnedRemote is an exact read of a verified provider response identity.
// Ownership, issuer, readiness and expiry remain part of the SQL predicate;
// callers cannot use this method to discover or register workspace resources.
func (s *Service) FindOwnedRemote(ctx context.Context, owner core.ResourceOwner, binding core.ResourceBinding, kind, id string) (core.ProviderResource, error) {
	if !validOwner(owner) || !validBinding(binding) || !bounded(kind) || !bounded(id) {
		return core.ProviderResource{}, core.ErrInvalidArgument
	}
	r, _, err := read(store.G(ctx, s.db.Gorm), ownedRemote, owner.UserID, owner.GroupID, kind, binding.AccountID, binding.PrincipalID, binding.Generation, id)
	if store.IsNoRows(err) {
		err = core.ErrNotFound
	}
	return r, err
}

// ownedRemote keeps the ready, unexpired ccgateway resource of an owner
// (user, group) under a remote identity (kind, account, principal,
// generation, remote ID).
const ownedRemote = `user_id=? AND group_id=? AND plugin_key='ccgateway' AND kind=? AND account_id=? AND principal_id=? AND generation=? AND remote_id=? AND state='ready' AND (expires_at IS NULL OR expires_at>now())`
