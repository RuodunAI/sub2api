package event

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the outbox insert built with GORM (see package gormtest): one
// event binds its values, more go as two arrays in one text for any count.
func TestSQLEventInsert(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertEvent(db, "balance.changed", []byte(`{"user_id":7}`)) })
	})
	t.Run("two", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return insertEvents(db, store.Strings{"balance.changed", "user.updated"},
				store.Array[store.Bytes]{store.Bytes(`{"user_id":7}`), store.Bytes(`{}`)})
		})
	})
}
