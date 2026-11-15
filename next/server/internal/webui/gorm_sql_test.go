package webui

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the shared asset statements built with GORM (see package
// gormtest).

// The file writes of Publish: every blob and its place in the build, in one
// statement of array parameters.
func TestSQLAssetPut(t *testing.T) {
	var paths []string
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return putAssets(db, "b1", []publicFile{
			{name: "assets/a.js", hash: "h", body: []byte("x")},
			{name: "assets/b.css", hash: "i", body: []byte{}},
		}).Scan(&paths)
	})
}

func TestSQLAssetRenewBuild(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return renewBuild(db, "b1", 90)
	})
}

func TestSQLAssetPublishBuild(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Exec(publishBuildSQL, "b1", 90.0)
	})
}

func TestSQLAssetCollectBuilds(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Exec(collectBuildsSQL, 604800.0, 3)
	})
}

func TestSQLAssetStoreUsage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var usage struct{ Size, Files, Builds int64 }
		return storeUsage(db).Scan(&usage)
	})
}
