package providerresources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type remoteIdentity struct {
	Plugin, Kind string
	Binding      core.ResourceBinding
	ID           string
}

func remoteKey(plugin, kind string, binding core.ResourceBinding, id string) string {
	raw, _ := json.Marshal(remoteIdentity{plugin, kind, binding, id})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func lockRemote(ctx context.Context, tx *gorm.DB, plugin, kind string, binding core.ResourceBinding, id string) error {
	return store.XactLock(ctx, tx, "provider-resource-identity:"+remoteKey(plugin, kind, binding, id))
}
func lookupRemote(ctx context.Context, q *gorm.DB, plugin, kind string, binding core.ResourceBinding, id string) ([]core.ProviderResource, error) {
	var rows []model.ProviderResource
	err := resources(store.G(ctx, q)).Where(`plugin_key=? AND kind=? AND account_id=? AND principal_id=? AND generation=? AND remote_id=?`, plugin, kind, binding.AccountID, binding.PrincipalID, binding.Generation, id).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return resourceList(rows), nil
}

func observedExpiry(in core.ResourceObservation, current *core.ProviderResource, now time.Time) (time.Time, error) {
	var expiry time.Time
	if in.ExpiresAt != nil && !in.ExpiresAt.IsZero() {
		expiry = in.ExpiresAt.UTC().Truncate(time.Microsecond)
	}
	if in.Kind == "container" && expiry.IsZero() {
		return time.Time{}, core.ErrInvalidArgument.WithMessage("container requires a verified provider expiry")
	}
	if in.Kind == "container" && (in.RequestStartedAt.IsZero() || in.RequestStartedAt.After(now)) {
		return time.Time{}, core.ErrInvalidArgument.WithMessage("container observation requires a trusted dispatch time")
	}
	if current == nil {
		if !expiry.IsZero() && !expiry.After(now) {
			return time.Time{}, core.ErrConflict.WithMessage("observed resource has already expired")
		}
		return expiry, nil
	}
	if current.State != "ready" {
		return time.Time{}, core.ErrConflict.WithMessage("resource is not available for observation")
	}
	if in.Kind == "container" {
		if in.RequestStartedAt.IsZero() || in.RequestStartedAt.After(now) || current.ExpiresAt.IsZero() || !current.ExpiresAt.After(in.RequestStartedAt) {
			return time.Time{}, core.ErrConflict.WithMessage("container was not valid at the verified dispatch time")
		}
		if current.ExpiresAt.After(expiry) {
			expiry = current.ExpiresAt
		}
		if !expiry.After(now) {
			return time.Time{}, core.ErrConflict.WithMessage("observed container remains expired")
		}
		return expiry, nil
	}
	if !current.ExpiresAt.IsZero() && !current.ExpiresAt.After(now) {
		return time.Time{}, core.ErrConflict.WithMessage("expired file cannot be revived")
	}
	if in.ExpiresAt == nil || expiry.IsZero() {
		return current.ExpiresAt, nil
	}
	if !current.ExpiresAt.IsZero() && current.ExpiresAt.Before(expiry) {
		return current.ExpiresAt, nil
	}
	if !expiry.After(now) {
		return time.Time{}, core.ErrConflict.WithMessage("observed file has expired")
	}
	return expiry, nil
}

// observedRow is the ready provider_resources row that registers the
// observation in under the public ID id, by operation op; key is its
// remote identity key.
func observedRow(id, op, key string, in core.ResourceObservation, meta json.RawMessage, expires *time.Time) *model.ProviderResource {
	return &model.ProviderResource{
		PublicID: id, UserID: in.Owner.UserID, GroupID: in.Owner.GroupID, RequestID: "observed:" + key, PluginKey: in.PluginKey, Kind: in.Kind,
		AccountID: in.Binding.AccountID, PrincipalID: in.Binding.PrincipalID, Generation: in.Binding.Generation, RemoteID: in.RemoteID,
		State: "ready", OperationID: op, Bytes: in.Bytes, Metadata: meta, IntentHash: key, ExpiresAt: expires,
	}
}

func (s *Service) RegisterObserved(ctx context.Context, in core.ResourceObservation) (out core.ProviderResource, err error) {
	if !validOwner(in.Owner) || !validBinding(in.Binding) || !bounded(in.PluginKey) || !bounded(in.RemoteID) || (in.Kind != "file" && in.Kind != "container") || in.Bytes < 0 || in.Bytes > s.limits.MaxResourceBytes {
		return out, core.ErrInvalidArgument
	}
	meta, err := metadata(in.Metadata)
	if err != nil {
		return out, err
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if e := lockOwner(ctx, tx, in.Owner); e != nil {
			return e
		}
		if e := lockRemote(ctx, tx, in.PluginKey, in.Kind, in.Binding, in.RemoteID); e != nil {
			return e
		}
		rows, e := lookupRemote(ctx, tx, in.PluginKey, in.Kind, in.Binding, in.RemoteID)
		if e != nil {
			return e
		}
		if len(rows) > 1 {
			return core.ErrConflict.WithMessage("remote resource identity has multiple historical records")
		}
		now := time.Now()
		if len(rows) == 1 {
			r := rows[0]
			if r.Owner != in.Owner {
				return core.ErrConflict.WithMessage("remote resource belongs to another owner")
			}
			// The owner advisory lock also serializes deletion/finalization.
			expiry, e := observedExpiry(in, &r, now)
			if e != nil {
				return e
			}
			if in.Kind == "file" && r.Bytes != in.Bytes {
				return core.ErrConflict.WithMessage("observed file size changed")
			}
			countDelta, bytesDelta := 0, in.Bytes-r.Bytes
			if !resourceOccupiesQuota(r, now) {
				countDelta, bytesDelta = 1, in.Bytes
			}
			if countDelta > 0 && in.Kind == "container" {
				if e = s.contextQuota(ctx, tx, in.Owner, 0, now, r.PublicID); e != nil {
					return e
				}
			}
			if e = s.quotaAt(ctx, tx, in.Owner, countDelta, bytesDelta, now); e != nil {
				return e
			}
			var expires *time.Time
			if !expiry.IsZero() {
				expires = &expiry
			}
			e = update(g, resourcesTable, r.PublicID, map[string]any{"metadata": meta, "bytes": in.Bytes, "expires_at": expires}).Error
			if e != nil {
				return e
			}
			out, _, e = read(g, `public_id=?`, r.PublicID)
			return e
		}
		expiry, e := observedExpiry(in, nil, now)
		if e != nil {
			return e
		}
		if e = s.quotaAt(ctx, tx, in.Owner, 1, in.Bytes, now); e != nil {
			return e
		}
		id, op := "s2res_"+uuid.NewString(), uuid.NewString()
		key := remoteKey(in.PluginKey, in.Kind, in.Binding, in.RemoteID)
		var expires *time.Time
		if !expiry.IsZero() {
			expires = &expiry
		}
		e = g.Create(observedRow(id, op, key, in, meta, expires)).Error
		if store.IsUniqueViolation(e, "") {
			return core.ErrConflict
		}
		if e != nil {
			return e
		}
		out, _, e = read(g, `public_id=?`, id)
		return e
	})
	return
}
