package helperhistory

import (
	"context"

	"gorm.io/gorm"

	wire "github.com/Sub2API-Devs/sup2api/next/plugins/ccgateway/companions/contracts/helperhistory"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) Reserve(ctx context.Context, in core.HelperHistoryReservation) (out core.HelperHistoryAttempt, err error) {
	if !validBinding(in.Binding) || !bounded(in.RequestID) || len(in.RequestID) > 64 || !wire.ValidDigest(in.RequestDigest) || !bounded(in.Namespace) || in.ReserveBytes <= 0 || in.ReserveBytes > wire.MaxPayloadBytes {
		return out, core.ErrInvalidArgument
	}
	err = s.transaction(ctx, in.Owner, func(tx *gorm.DB) error {
		now := s.now()
		if e := s.clean(ctx, tx, in.Owner, now); e != nil {
			return e
		}
		chain, e := s.resolve(ctx, tx, in.Owner, in.PriorPrefixes, in.Namespace, now)
		if e != nil {
			return e
		}
		parent := ""
		if len(chain.Records) > 0 {
			if chain.Binding != in.Binding {
				return core.ErrConflict.WithMessage("helper history issuer changed")
			}
			parent = chain.Records[len(chain.Records)-1].Receipt
		}
		db := store.G(ctx, tx)
		var prior model.ProviderHelperAttempt
		e = db.Select("id").Scopes(owned(in.Owner)).Where("request_id = ?", in.RequestID).Take(&prior).Error
		if e == nil {
			old, e := readAttempt(ctx, tx, in.Owner, prior.ID)
			if e != nil {
				return e
			}
			if old.requestDigest != in.RequestDigest || old.namespace != in.Namespace || old.Binding != in.Binding || old.ParentReceipt != parent || old.reservedBytes != in.ReserveBytes {
				return core.ErrConflict
			}
			if !old.ExpiresAt.After(now) {
				return core.ErrNotFound
			}
			out = old.HelperHistoryAttempt
			return nil
		}
		if !store.IsNoRows(e) {
			return e
		}
		if e = s.quota(ctx, tx, in.Owner, now, in.ReserveBytes); e != nil {
			return e
		}
		id, e := identifier()
		if e != nil {
			return e
		}
		out = core.HelperHistoryAttempt{ID: id, ParentReceipt: parent, State: "reserved", Binding: in.Binding, ExpiresAt: now.Add(s.options.Retention)}
		// A root attempt's empty parent_receipt is the column's default, so
		// GORM leaving the zero value out of the INSERT stores the same ''.
		return db.Create(&model.ProviderHelperAttempt{ID: id, UserID: in.Owner.UserID, GroupID: in.Owner.GroupID, RequestID: in.RequestID,
			RequestDigest: in.RequestDigest, Namespace: in.Namespace, AccountID: in.Binding.AccountID, PrincipalID: in.Binding.PrincipalID,
			Generation: in.Binding.Generation, ParentReceipt: parent, State: out.State, ReservedBytes: in.ReserveBytes, CreatedAt: now,
			ExpiresAt: out.ExpiresAt}).Error
	})
	return
}
func (s *Service) transition(ctx context.Context, o core.ResourceOwner, id, from, to string) error {
	if !bounded(id) {
		return core.ErrInvalidArgument
	}
	return s.transaction(ctx, o, func(tx *gorm.DB) error {
		a, e := readAttempt(ctx, tx, o, id)
		if e != nil {
			return e
		}
		if !a.ExpiresAt.After(s.now()) {
			return core.ErrNotFound
		}
		if a.State == to && to != "dispatched" {
			return nil
		}
		if a.State != from {
			return core.ErrConflict.WithMessage("helper history attempt cannot transition")
		}
		return setAttemptState(store.G(ctx, tx), id, to).Error
	})
}
func (s *Service) MarkDispatched(c context.Context, o core.ResourceOwner, id string) error {
	return s.transition(c, o, id, "reserved", "dispatched")
}
func (s *Service) MarkUncertain(c context.Context, o core.ResourceOwner, id string) error {
	return s.transition(c, o, id, "dispatched", "uncertain")
}
func (s *Service) Abort(c context.Context, o core.ResourceOwner, id string) error {
	return s.transition(c, o, id, "reserved", "aborted")
}
