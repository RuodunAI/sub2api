package providerresources

import (
	"bytes"
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) mutate(ctx context.Context, owner core.ResourceOwner, id string, fn func(*gorm.DB, core.ProviderResource) error) error {
	if !validOwner(owner) || !bounded(id) {
		return core.ErrInvalidArgument
	}
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockOwner(ctx, tx, owner); err != nil {
			return err
		}
		r, _, err := read(store.G(ctx, tx).Clauses(store.ForUpdate), `user_id=? AND group_id=? AND public_id=?`, owner.UserID, owner.GroupID, id)
		if store.IsNoRows(err) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		return fn(tx, r)
	})
}

func (s *Service) Finalize(ctx context.Context, in core.ResourceCompletion) (out core.ProviderResource, err error) {
	if !bounded(in.RemoteID) || !bounded(in.OperationID) || !validBinding(in.Binding) || in.Bytes < 0 || in.Bytes > s.limits.MaxResourceBytes {
		return out, core.ErrInvalidArgument
	}
	meta, err := metadata(in.Metadata)
	if err != nil {
		return out, err
	}
	err = s.mutate(ctx, in.Owner, in.PublicID, func(tx *gorm.DB, r core.ProviderResource) error {
		if r.Binding != in.Binding || r.OperationID != in.OperationID {
			return core.ErrConflict
		}
		expires := r.ExpiresAt
		if in.ExpiresAt != nil && !in.ExpiresAt.IsZero() {
			providerExpiry := in.ExpiresAt.UTC().Truncate(time.Microsecond)
			if expires.IsZero() || providerExpiry.Before(expires) {
				expires = providerExpiry
			}
		}
		if r.State == "ready" {
			old, _ := metadata(r.Metadata)
			if r.RemoteID != in.RemoteID || r.Bytes != in.Bytes || !bytes.Equal(old, meta) || !r.ExpiresAt.Equal(expires) {
				return core.ErrConflict
			}
			out = r
			return nil
		}
		if r.State != "pending" && r.State != "uncertain" {
			return core.ErrConflict
		}
		if e := lockRemote(ctx, tx, r.PluginKey, r.Kind, r.Binding, in.RemoteID); e != nil {
			return e
		}
		known, e := lookupRemote(ctx, tx, r.PluginKey, r.Kind, r.Binding, in.RemoteID)
		if e != nil {
			return e
		}
		for _, other := range known {
			if other.PublicID != r.PublicID {
				return core.ErrConflict.WithMessage("remote resource identity was previously registered")
			}
		}
		if err := s.quota(ctx, tx, in.Owner, 0, in.Bytes-r.Bytes); err != nil {
			return err
		}
		var storedExpiry *time.Time
		if !expires.IsZero() {
			storedExpiry = &expires
		}
		g := store.G(ctx, tx)
		err := update(g, resourcesTable, r.PublicID, map[string]any{
			"state": "ready", "remote_id": in.RemoteID, "bytes": in.Bytes, "metadata": meta, "expires_at": storedExpiry,
		}).Error
		if store.IsUniqueViolation(err, "") {
			return core.ErrConflict.WithMessage("remote resource already registered")
		}
		if err != nil {
			return err
		}
		out, _, err = read(g, `public_id=?`, r.PublicID)
		return err
	})
	return out, err
}

func (s *Service) MarkUncertain(ctx context.Context, owner core.ResourceOwner, id, op string) error {
	return s.mutate(ctx, owner, id, func(tx *gorm.DB, r core.ProviderResource) error {
		if r.OperationID != op || (r.State != "pending" && r.State != "uncertain") {
			return core.ErrConflict
		}
		return update(store.G(ctx, tx), resourcesTable, id, map[string]any{"state": "uncertain"}).Error
	})
}

// FailCreate accepts only recognized, authenticated provider rejection classes.
// The caller must establish that the response belongs to this exact operation;
// a status code alone, timeout, proxy error, or uncertain result is insufficient.
func (s *Service) FailCreate(ctx context.Context, owner core.ResourceOwner, id, op, evidenceCode string) error {
	switch evidenceCode {
	case "invalid_request_error", "authentication_error", "permission_error":
	default:
		return core.ErrInvalidArgument.WithMessage("creation failure needs confirmed provider rejection evidence")
	}
	return s.mutate(ctx, owner, id, func(tx *gorm.DB, r core.ProviderResource) error {
		if r.OperationID != op || (r.State != "pending" && r.State != "uncertain" && r.State != "failed") {
			return core.ErrConflict
		}
		g := store.G(ctx, tx)
		if r.State == "failed" {
			var old model.ProviderResource
			if err := g.Select("failure_code").Where("public_id = ?", id).Take(&old).Error; err != nil {
				return err
			}
			if old.FailureCode != evidenceCode {
				return core.ErrConflict
			}
			return nil
		}
		return update(g, resourcesTable, id, map[string]any{"state": "failed", "failure_code": evidenceCode}).Error
	})
}

func (s *Service) BeginDelete(ctx context.Context, owner core.ResourceOwner, id string, binding core.ResourceBinding) (out core.ResourceReservation, err error) {
	err = s.mutate(ctx, owner, id, func(tx *gorm.DB, r core.ProviderResource) error {
		if r.Binding != binding {
			return core.ErrConflict.WithMessage("resource issuer identity changed")
		}
		out.Resource = r
		if r.State != "ready" {
			return nil
		}
		op := uuid.NewString()
		if e := update(store.G(ctx, tx), resourcesTable, id, map[string]any{"state": "deleting", "operation_id": op}).Error; e != nil {
			return e
		}
		out.Resource.State = "deleting"
		out.Resource.OperationID = op
		out.Dispatch = true
		return nil
	})
	return
}

// A failed or timed out delete is not evidence that the remote object vanished.
// Retrying requires an explicit reconciliation operation, not BeginDelete.
func (s *Service) FinishDelete(ctx context.Context, owner core.ResourceOwner, id, op string, confirmed bool) error {
	return s.mutate(ctx, owner, id, func(tx *gorm.DB, r core.ProviderResource) error {
		if r.OperationID != op {
			return core.ErrConflict
		}
		if r.State == "deleted" && confirmed {
			return nil
		}
		if r.State != "deleting" && r.State != "delete_uncertain" {
			return core.ErrConflict
		}
		state := "delete_uncertain"
		if confirmed {
			state = "deleted"
		}
		return update(store.G(ctx, tx), resourcesTable, id, map[string]any{"state": state}).Error
	})
}
