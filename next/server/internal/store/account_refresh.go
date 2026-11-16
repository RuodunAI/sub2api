package store

import (
	"context"
	"database/sql"
	"time"

	"gorm.io/gorm"
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
	err := EachRow(G(ctx, d.Gorm).Raw(`SELECT account_id, expires_at, last_attempt_at, last_success_at, error_type, error, rejected_cred_hash
		FROM account_credential_refresh WHERE account_id = ANY(?)`, Int64s(ids)), func(rows *sql.Rows) error {
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
	return db.Exec(`INSERT INTO account_credential_refresh
			(account_id, expires_at, last_attempt_at, last_success_at, error_type, error, rejected_cred_hash)
		VALUES (?, ?, now(), CASE WHEN ? = '' THEN now() END, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET
			expires_at = COALESCE(EXCLUDED.expires_at, account_credential_refresh.expires_at),
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = COALESCE(EXCLUDED.last_success_at, account_credential_refresh.last_success_at),
			error_type = EXCLUDED.error_type,
			error = EXCLUDED.error,
			rejected_cred_hash = EXCLUDED.rejected_cred_hash`,
		s.AccountID, s.ExpiresAt, s.ErrorType, s.ErrorType, s.Error, s.RejectedCredHash)
}
