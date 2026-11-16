package environment

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the environment statements (testdata/*.golden); the
// package has no others (the PUT goes through store.UpdateSettingJSONTx).

func TestSQLLayers(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return layersQuery(db, 3, 9, &layerRows{}) })
}
