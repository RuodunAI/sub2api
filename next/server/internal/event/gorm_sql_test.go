package event

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the outbox insert built with GORM (see package gormtest): one
// row per event, in their order.
func TestSQLEventInsert(t *testing.T) {
	for name, args := range map[string][]any{
		"one": {"balance.changed", []byte(`{"user_id":7}`)},
		"two": {"balance.changed", []byte(`{"user_id":7}`), "user.updated", []byte(`{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return insertEvents(db, args) })
		})
	}
}
