package core

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the core statements built with GORM (see package gormtest).

func TestSQLLockOpenDrafts(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return lockOpenDrafts(db, 7) })
}
