package helperhistory

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type attempt struct {
	core.HelperHistoryAttempt
	owner                                            core.ResourceOwner
	requestID, requestDigest, namespace, usageDigest string
	reservedBytes                                    int64
}

// owned keeps the rows of owner o; every read of this package goes through
// it, so no statement crosses owners.
func owned(o core.ResourceOwner) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB { return db.Where("user_id = ? AND group_id = ?", o.UserID, o.GroupID) }
}

func readAttempt(ctx context.Context, q store.Querier, o core.ResourceOwner, id string) (attempt, error) {
	var row model.ProviderHelperAttempt
	if e := store.G(ctx, q).Scopes(owned(o)).Where("id = ?", id).Take(&row).Error; e != nil {
		if store.IsNoRows(e) {
			e = core.ErrNotFound
		}
		return attempt{}, e
	}
	return attempt{
		HelperHistoryAttempt: core.HelperHistoryAttempt{ID: row.ID, ParentReceipt: row.ParentReceipt, State: row.State,
			Binding: core.ResourceBinding{AccountID: row.AccountID, PrincipalID: row.PrincipalID, Generation: row.Generation}, ExpiresAt: row.ExpiresAt},
		owner: o, requestID: row.RequestID, requestDigest: row.RequestDigest, namespace: row.Namespace, usageDigest: row.UsageDigest,
		reservedBytes: row.ReservedBytes,
	}, nil
}
func readRecord(ctx context.Context, q store.Querier, o core.ResourceOwner, id string) (core.HelperHistoryRecord, string, error) {
	var row model.ProviderHelperRecord
	if e := store.G(ctx, q).Scopes(owned(o)).Where("receipt = ?", id).Take(&row).Error; e != nil {
		if store.IsNoRows(e) {
			e = core.ErrNotFound
		}
		return core.HelperHistoryRecord{}, "", e
	}
	return core.HelperHistoryRecord{Receipt: row.Receipt, ParentReceipt: row.ParentReceipt, PublicPrefixDigest: row.PublicPrefixDigest,
		ChainDigest: row.ChainDigest, Namespace: row.Namespace,
		Binding: core.ResourceBinding{AccountID: row.AccountID, PrincipalID: row.PrincipalID, Generation: row.Generation},
		Payload: row.Payload, ExpiresAt: row.ExpiresAt}, row.PayloadDigest, nil
}

// setAttemptState moves attempt id to state; the caller holds the owner's
// lock and has read the attempt through owned.
func setAttemptState(db *gorm.DB, id, state string) *gorm.DB {
	return db.Model(&model.ProviderHelperAttempt{}).Where("id = ?", id).Update("state", state)
}

// prefixRow is one record that prefixRecord weighs.
type prefixRow struct {
	Receipt     string
	ChainDigest string
	ExpiresAt   time.Time
	Present     bool // the payload is still there
}

// prefixRows reads into dst the records of o under namespace and prefix,
// oldest first.
func prefixRows(db *gorm.DB, o core.ResourceOwner, namespace, prefix string, dst *[]prefixRow) *gorm.DB {
	return db.Model(&model.ProviderHelperRecord{}).Select("receipt, chain_digest, expires_at, payload IS NOT NULL AS present").
		Scopes(owned(o)).Where("namespace = ? AND public_prefix_digest = ?", namespace, prefix).
		Order("created_at, receipt").Scan(dst)
}

func prefixRecord(ctx context.Context, q store.Querier, o core.ResourceOwner, namespace, prefix string, now time.Time) (string, error) {
	var rows []prefixRow
	if e := prefixRows(store.G(ctx, q), o, namespace, prefix, &rows).Error; e != nil {
		return "", e
	}
	id, digest := "", ""
	expired := false
	for _, r := range rows {
		if digest != "" && digest != r.ChainDigest {
			return "", core.ErrConflict.WithMessage("ambiguous helper history prefix")
		}
		digest = r.ChainDigest
		if !r.Present || !r.ExpiresAt.After(now) {
			expired = true
		}
		if id == "" {
			id = r.Receipt
		}
	}
	if id == "" || expired {
		return "", core.ErrNotFound.WithMessage("helper history missing or expired")
	}
	return id, nil
}
