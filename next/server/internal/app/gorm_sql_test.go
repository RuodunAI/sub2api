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
