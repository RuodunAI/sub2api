package webui

import (
	"database/sql"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the shared asset statements built with GORM (see package
// gormtest).

// The file writes of Publish: the blobs and the stored hashes read back.
// DryRun reads none, a collision, so the run stops before the files of the
// build.
func TestSQLAssetPut(t *testing.T) {
	gormtest.AssertRun(t, func(db *gorm.DB) error {
		err := putAssets(db, "b1", []publicFile{
			{name: "assets/a.js", hash: "h", body: []byte("x")},
			{name: "assets/b.css", hash: "i", body: []byte{}},
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
}

func TestSQLAssetRenewBuild(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return renewBuild(db, "b1", 90)
	})
}

func TestSQLAssetStoreUsage(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var usage struct{ Size, Files, Builds int64 }
		return storeUsage(db).Scan(&usage)
	})
}
