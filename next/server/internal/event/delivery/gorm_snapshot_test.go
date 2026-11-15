package delivery

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL snapshots of the delivery statements (see package gormtest).

// advance's write: the reset values (0, NULL, the empty string) must be in
// the SET list, and the cursor must still be where it was read.
func TestSnapshotAdvanceCursor(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return updateCursor(tx, "demo", 41, map[string]any{
			"last_event_id": int64(50), "consecutive_failures": 0, "next_retry_at": nil, "last_error": "",
		})
	})
}

func TestSnapshotCreateCursor(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return createCursor(tx, "demo") })
}

// The id list is one bigint[] parameter.
func TestSnapshotLoadEvents(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var rows []model.Event
		return loadEvents(tx, []int64{7, 9, 12}, &rows)
	})
}

func TestSnapshotPurgeChunk(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return purgeChunk(tx, 7*24*time.Hour) })
}
