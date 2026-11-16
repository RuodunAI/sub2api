package notify

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL snapshots of the notification statements (testdata/*.golden).

func TestSQLHeldDedupeKey(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return heldDedupeKey(db, "core", "quota:7", 600, &model.Notification{})
	})
}

func TestSQLClaim(t *testing.T) {
	s := New(nil, nil, Options{})
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return s.claim(db, &model.Notification{}) })
}

func TestSQLRecordUnbound(t *testing.T) {
	s := New(nil, nil, Options{})
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return s.record(db, queued{id: 9, attempt: 3}, errUnbound, false)
	})
}

func TestSQLRecordRetry(t *testing.T) {
	s := New(nil, nil, Options{})
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return s.record(db, queued{id: 9, attempt: 2}, errors.New("smtp: 451"), false)
	})
}

func TestSQLRecordPage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return recordPage(db, "failed", "", 20, 40, &[]model.Notification{})
	})
}
