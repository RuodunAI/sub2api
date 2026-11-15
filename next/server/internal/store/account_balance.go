package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Account balance snapshots (table account_balances, CONTRACTS §51).

// Values of BalanceSnapshot.Source.
const (
	BalanceActive = "active"
)

// BalanceSnapshot is one account_balances row. The JSON form is the one
// the account service caches in Redis (CONTRACTS §51).
type BalanceSnapshot struct {
	AccountID    int64      `json:"account_id"`
	AmountMicros int64      `json:"amount_micros"`
	Currency     string     `json:"currency"`
	Source       string     `json:"source"`
	UpdatedAt    *time.Time `json:"updated_at"`
	Error        string     `json:"error"`
	ErrorAt      *time.Time `json:"error_at"`
}

const selectBalance = `SELECT account_id, amount_micros, currency, source, updated_at, error, error_at
	FROM account_balances`

func scanBalance(r interface{ Scan(...any) error }) (*BalanceSnapshot, error) {
	var b BalanceSnapshot
	if err := r.Scan(&b.AccountID, &b.AmountMicros, &b.Currency, &b.Source, &b.UpdatedAt, &b.Error, &b.ErrorAt); err != nil {
		return nil, err
	}
	return &b, nil
}

// AccountBalance returns the snapshot of one account, nil when it has none.
func (d *DB) AccountBalance(ctx context.Context, accountID int64) (*BalanceSnapshot, error) {
	b, err := scanBalance(G(ctx, d.Pool).Raw(selectBalance+` WHERE account_id = ?`, accountID).Row())
	if IsNoRows(err) {
		return nil, nil
	}
	return b, err
}

// AccountBalances returns the snapshots of the given accounts in one query;
// accounts without one are absent from the map.
func (d *DB) AccountBalances(ctx context.Context, accountIDs []int64) (map[int64]*BalanceSnapshot, error) {
	out := map[int64]*BalanceSnapshot{}
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := G(ctx, d.Pool).Raw(selectBalance+` WHERE account_id = ANY(?)`, Int64s(accountIDs)).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBalance(rows)
		if err != nil {
			return nil, err
		}
		out[b.AccountID] = b
	}
	return out, rows.Err()
}

// UpsertBalance writes one balance snapshot.
func (d *DB) UpsertBalance(ctx context.Context, b *BalanceSnapshot) error {
	return upsertBalance(G(ctx, d.Pool), b).Error
}

func upsertBalance(db *gorm.DB, b *BalanceSnapshot) *gorm.DB {
	return db.Exec(`
		INSERT INTO account_balances (account_id, amount_micros, currency, source, updated_at, error, error_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET
			amount_micros = EXCLUDED.amount_micros,
			currency = EXCLUDED.currency,
			source = EXCLUDED.source,
			updated_at = EXCLUDED.updated_at,
			error = EXCLUDED.error,
			error_at = EXCLUDED.error_at
	`, b.AccountID, b.AmountMicros, b.Currency, b.Source, b.UpdatedAt, b.Error, b.ErrorAt)
}

// DeleteBalance removes the balance snapshot of one account.
func (d *DB) DeleteBalance(ctx context.Context, accountID int64) error {
	return G(ctx, d.Pool).Exec(`DELETE FROM account_balances WHERE account_id = ?`, accountID).Error
}
