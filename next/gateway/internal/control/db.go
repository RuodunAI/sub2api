package control

import (
	"database/sql"
	"database/sql/driver"
	"errors"

	"gorm.io/gorm"
)

// isNoRows reports a missing row on either GORM path: Raw(...).Row().Scan
// returns sql.ErrNoRows, First and Take return gorm.ErrRecordNotFound.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound)
}

// textArray is one text[] parameter (= ANY(?)). GORM expands a plain slice
// into a list of placeholders; a driver.Valuer stays one parameter, and pgx
// encodes the []string as an array.
type textArray []string

// Value implements driver.Valuer.
func (a textArray) Value() (driver.Value, error) { return []string(a), nil }

// eventRow is one updater.events row; its id and created_at are database
// defaults.
type eventRow struct {
	UpgradeID string
	Kind      string
	Message   string
}

func (eventRow) TableName() string { return "updater.events" }

// insertEvents journals rows in one INSERT.
func insertEvents(db *gorm.DB, rows ...eventRow) error {
	return db.Create(rows).Error
}
