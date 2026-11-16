package store

import (
	"context"
	"database/sql"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Credential refresh state of accounts (table account_credential_refresh,
// CONTRACTS §48).

// Values of RefreshState.ErrorType.
const (
	RefreshAuthRejected = "auth_rejected"
	RefreshTransient    = "transient"
)

// RefreshState is one account_credential_refresh row.
type RefreshState struct {
	AccountID        int64
	ExpiresAt        *time.Time
	LastAttemptAt    *time.Time
	LastSuccessAt    *time.Time
	ErrorType        string
	Error            string
	RejectedCredHash []byte
}

// AccountRefreshStates returns the refresh state of the given accounts;
// accounts without a row are absent from the map.
func (d *DB) AccountRefreshStates(ctx context.Context, ids []int64) (map[int64]*RefreshState, error) {
	out := map[int64]*RefreshState{}
	if len(ids) == 0 {
		return out, nil
	}
	err := EachRow(G(ctx, d.Gorm).Table(refreshTable).
		Select("account_id, expires_at, last_attempt_at, last_success_at, error_type, error, rejected_cred_hash").
		Where("account_id = ANY(?)", Int64s(ids)), func(rows *sql.Rows) error {
		var s RefreshState
		if err := rows.Scan(&s.AccountID, &s.ExpiresAt, &s.LastAttemptAt, &s.LastSuccessAt, &s.ErrorType, &s.Error, &s.RejectedCredHash); err != nil {
			return err
		}
		out[s.AccountID] = &s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordAccountRefresh stores the outcome of one refresh attempt (in tx
// when the credentials were saved with it). A
// successful attempt (ErrorType "") sets last_success_at and clears the
// error and the rejected hash.
func RecordAccountRefresh(ctx context.Context, q *gorm.DB, s RefreshState) error {
	return recordAccountRefresh(G(ctx, q), s).Error
}

func recordAccountRefresh(db *gorm.DB, s RefreshState) *gorm.DB {
	return db.Table(refreshTable).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}},
		DoUpdates: append(clause.AssignmentColumns([]string{"last_attempt_at", "error_type", "error", "rejected_cred_hash"}),
			clause.Assignments(map[string]any{
				"expires_at":      gorm.Expr("COALESCE(EXCLUDED.expires_at, account_credential_refresh.expires_at)"),
				"last_success_at": gorm.Expr("COALESCE(EXCLUDED.last_success_at, account_credential_refresh.last_success_at)"),
			})...),
	}).Create(map[string]any{"account_id": s.AccountID, "expires_at": s.ExpiresAt, "last_attempt_at": gorm.Expr("now()"),
		"last_success_at": gorm.Expr("CASE WHEN ? = '' THEN now() END", s.ErrorType), "error_type": s.ErrorType, "error": s.Error,
		"rejected_cred_hash": s.RejectedCredHash})
}

const refreshTable = "account_credential_refresh"
