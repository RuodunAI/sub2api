package event

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the outbox insert built with GORM (see package gormtest): one
// event takes the next id as it is written, more reserve their ids first
// and write them with the rows.
func TestSQLEventInsert(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		gormtest.AssertRun(t, func(db *gorm.DB) error {
			return insertEvents(db, []model.Event{{Type: "balance.changed", Payload: []byte(`{"user_id":7}`)}})
		})
	})
	two := []model.Event{
		{Type: "balance.changed", Payload: []byte(`{"user_id":7}`)},
		{Type: "user.updated", Payload: []byte(`{}`)},
	}
	t.Run("two", func(t *testing.T) {
		// DryRun reserves no ids, so the run ends at NextIDs.
		gormtest.AssertRun(t, func(db *gorm.DB) error { return insertEvents(db, two) })
	})
	t.Run("two-reserved", func(t *testing.T) {
		two[0].ID, two[1].ID = 41, 42
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Create(&two) })
	})
}
