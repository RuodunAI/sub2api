package app

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the readiness checks built with GORM (see package gormtest).

func TestSQLReadyPluginStates(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var plugins []pluginState
		return pluginStates(db).Find(&plugins)
	})
}

func TestSQLReadyAdmission(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var approved bool
		return db.Raw(admissionSQL, "node-1", "boot-1", "sha256:abc", int64(4), true, false, true).Scan(&approved)
	})
}

func TestSQLReadyVersionApproved(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var approved bool
		return db.Raw(versionApprovedSQL, "openai", "1.2.0").Scan(&approved)
	})
}
