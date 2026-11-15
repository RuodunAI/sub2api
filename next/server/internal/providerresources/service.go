// Package providerresources stores tenant-owned remote resource intents. It
// never performs network operations or retries an ambiguous remote side effect.
package providerresources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type Options struct {
	MaxResources               int
	MaxContexts                int
	MaxBytes, MaxResourceBytes int64
	DefaultTTL, MaxTTL         time.Duration
}
type Service struct {
	db     *store.DB
	limits Options
}

var _ core.ProviderResources = (*Service)(nil)

func New(db *store.DB, opts Options) *Service {
	if opts.MaxContexts <= 0 {
		opts.MaxContexts = 100000
	}
	if opts.MaxResources <= 0 {
		opts.MaxResources = 1000
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 10 << 30
	}
	if opts.MaxResourceBytes <= 0 {
		opts.MaxResourceBytes = 512 << 20
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = 0
	}
	if opts.MaxTTL <= 0 {
		opts.MaxTTL = 365 * 24 * time.Hour
	}
	return &Service{db: db, limits: opts}
}

func validOwner(o core.ResourceOwner) bool { return o.UserID > 0 && o.GroupID > 0 }
func bounded(s string) bool                { return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n") }
func validBinding(b core.ResourceBinding) bool {
	return b.AccountID > 0 && bounded(b.PrincipalID) && bounded(b.Generation)
}
func metadata(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if len(raw) > 16384 || !json.Valid(raw) || dec.Decode(&obj) != nil || obj == nil {
		return nil, core.ErrInvalidArgument.WithMessage("resource metadata must be a bounded JSON object")
	}
	return json.Marshal(obj)
}

func (s *Service) normalize(in core.ResourceIntent) (core.ResourceIntent, string, error) {
	if !validOwner(in.Owner) || !validBinding(in.Binding) || !bounded(in.RequestID) || !bounded(in.PluginKey) || !bounded(in.Kind) || in.Bytes < 0 || in.Bytes > s.limits.MaxResourceBytes {
		return in, "", core.ErrInvalidArgument
	}
	if in.TTL == 0 {
		in.TTL = s.limits.DefaultTTL
	}
	if in.TTL < 0 || in.TTL > s.limits.MaxTTL {
		return in, "", core.ErrInvalidArgument.WithMessage("invalid resource TTL")
	}
	var err error
	if in.Metadata, err = metadata(in.Metadata); err != nil {
		return in, "", err
	}
	raw, _ := json.Marshal(in)
	h := sha256.Sum256(raw)
	return in, hex.EncodeToString(h[:]), nil
}

func lockOwner(ctx context.Context, tx *gorm.DB, owner core.ResourceOwner) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	// A transaction-scoped lock serializes reservations/finalization for this quota.
	raw, _ := json.Marshal(owner)
	return advisoryLock(store.G(ctx, tx), "provider-resources:"+string(raw)).Error
}

// advisoryLock takes the transaction-scoped advisory lock of key.
func advisoryLock(db *gorm.DB, key string) *gorm.DB {
	return db.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, key)
}

func (s *Service) quota(ctx context.Context, tx *gorm.DB, owner core.ResourceOwner, extraCount int, extraBytes int64) error {
	return s.quotaAt(ctx, tx, owner, extraCount, extraBytes, time.Now())
}

func resourceOccupiesQuota(r core.ProviderResource, at time.Time) bool {
	return r.State != "deleted" && r.State != "failed" && !(r.Kind == "container" && !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(at))
}

func (s *Service) quotaAt(ctx context.Context, tx *gorm.DB, owner core.ResourceOwner, extraCount int, extraBytes int64, at time.Time) error {
	var count, bytes int64
	err := occupied(store.G(ctx, tx), owner, at).Row().Scan(&count, &bytes)
	if err != nil {
		return err
	}
	if count+int64(extraCount) > int64(s.limits.MaxResources) || extraBytes > s.limits.MaxBytes-bytes {
		return core.ErrRateLimited.WithMessage("provider resource quota exceeded")
	}
	return nil
}

// occupied counts the resources and skill versions of owner that occupy its
// quota at at, and sums their bytes.
func occupied(db *gorm.DB, owner core.ResourceOwner, at time.Time) *gorm.DB {
	return db.Raw(`SELECT count(*),COALESCE(sum(bytes),0) FROM (
 SELECT bytes FROM provider_resources WHERE user_id=@user AND group_id=@group AND state NOT IN ('deleted','failed') AND NOT(kind='container' AND expires_at IS NOT NULL AND expires_at<=@at)
 UNION ALL SELECT v.bytes FROM provider_skill_versions v JOIN provider_resources p ON p.public_id=v.parent_id WHERE p.user_id=@user AND p.group_id=@group AND p.state NOT IN ('deleted','failed') AND v.state NOT IN ('deleted','failed')
 ) occupied`, map[string]any{"user": owner.UserID, "group": owner.GroupID, "at": at})
}

func (s *Service) Reserve(ctx context.Context, input core.ResourceIntent) (out core.ResourceReservation, err error) {
	in, hash, err := s.normalize(input)
	if err != nil {
		return out, err
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockOwner(ctx, tx, in.Owner); err != nil {
			return err
		}
		g := store.G(ctx, tx)
		existing, oldHash, e := read(g, `user_id=? AND group_id=? AND request_id=?`, in.Owner.UserID, in.Owner.GroupID, in.RequestID)
		if e == nil {
			if oldHash != hash {
				return core.ErrConflict.WithMessage("resource request ID already has a different intent")
			}
			out.Resource = existing
			return nil
		}
		if !store.IsNoRows(e) {
			return e
		}
		if e = s.quota(ctx, tx, in.Owner, 1, in.Bytes); e != nil {
			return e
		}
		var expiry *time.Time
		if in.TTL > 0 {
			v := time.Now().Add(in.TTL)
			expiry = &v
		}
		if !in.ExpiresAt.IsZero() && (expiry == nil || in.ExpiresAt.Before(*expiry)) {
			v := in.ExpiresAt
			expiry = &v
		}
		id, op := "s2res_"+uuid.NewString(), uuid.NewString()
		row := newRow(id, op, in, "pending")
		row.IntentHash, row.ExpiresAt = hash, expiry
		if e = g.Create(&row).Error; e != nil {
			return e
		}
		out.Resource, _, e = read(g, `public_id=?`, id)
		out.Dispatch = e == nil
		return e
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, owner core.ResourceOwner, id string) (core.ProviderResource, error) {
	if !validOwner(owner) || !bounded(id) {
		return core.ProviderResource{}, core.ErrInvalidArgument
	}
	r, _, err := read(store.G(ctx, s.db.Gorm), `user_id=? AND group_id=? AND public_id=?`, owner.UserID, owner.GroupID, id)
	if store.IsNoRows(err) {
		err = core.ErrNotFound
	}
	return r, err
}

// newRow is the provider_resources row of intent in, created in state by
// operation op under the public ID id; the caller sets what in does not
// say (remote ID, intent hash, expiry).
func newRow(id, op string, in core.ResourceIntent, state string) model.ProviderResource {
	return model.ProviderResource{
		PublicID: id, UserID: in.Owner.UserID, GroupID: in.Owner.GroupID, RequestID: in.RequestID, PluginKey: in.PluginKey, Kind: in.Kind,
		AccountID: in.Binding.AccountID, PrincipalID: in.Binding.PrincipalID, Generation: in.Binding.Generation,
		State: state, OperationID: op, Bytes: in.Bytes, Metadata: in.Metadata,
	}
}

// List includes pending/uncertain states for reconciliation. Public file lists
// must select only ready, unexpired resources before projecting API objects.
func (s *Service) List(ctx context.Context, owner core.ResourceOwner, after string, limit int) ([]core.ProviderResource, error) {
	if !validOwner(owner) || limit < 1 || limit > 100 || len(after) > 256 {
		return nil, core.ErrInvalidArgument
	}
	var rows []model.ProviderResource
	if err := resources(store.G(ctx, s.db.Gorm)).Where(`user_id=? AND group_id=? AND public_id>?`, owner.UserID, owner.GroupID, after).
		Order("public_id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return resourceList(rows), nil
}

// resourceList is the ProviderResources rows hold, never nil.
func resourceList(rows []model.ProviderResource) []core.ProviderResource {
	out := make([]core.ProviderResource, len(rows))
	for i, row := range rows {
		out[i] = resource(row)
	}
	return out
}
