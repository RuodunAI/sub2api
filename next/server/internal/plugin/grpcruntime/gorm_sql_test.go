package grpcruntime

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the runtime statements built with GORM (see package gormtest).

func TestSQLRuntimeSettings(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row model.Plugin
		return pluginSettings(db, "demo").Take(&row)
	})
}
