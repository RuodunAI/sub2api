package event

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the outbox insert built with GORM (see package gormtest): one
// INSERT, its rows in the batch order, nothing read back.
func TestSQLEventInsert(t *testing.T) {
	one := map[string]any{"type": "balance.changed", "payload": []byte(`{"user_id":7}`)}
	t.Run("one", func(t *testing.T) {
		gormtest.AssertRun(t, func(db *gorm.DB) error { return insertEvents(db, []map[string]any{one}) })
	})
	t.Run("two", func(t *testing.T) {
		gormtest.AssertRun(t, func(db *gorm.DB) error {
			return insertEvents(db, []map[string]any{one, {"type": "user.updated", "payload": []byte(`{}`)}})
		})
	})
}
