package control

import "gorm.io/gorm"

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
