package volcengine

import (
	"database/sql"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"

	"github.com/Sub2API-Devs/sup2api/next/plugins/volcengine/internal/model"
)

// indexResource describes one indexed resource (a group or an asset) to
// withResource: the row locks to take in order and how to read the row once
// they are held. Group and asset differ only in these three values, so the
// transaction wrapper is written once.
type indexResource[R any] struct {
	// locks build the SELECT ... FOR UPDATE statements of the row id, run
	// in order. Taking every lock before reading is what orders concurrent
	// writers.
	locks []func(tx *gorm.DB, id int64) *gorm.DB
	// load reads the locked row; ErrNoRow means "gone".
	load func(*gorm.DB, int64) (R, error)
	// missing is the 404 body when the row is not in the index.
	missing string
}

// withResource keeps the upstream operation and its index write under the
// resource's row locks. An optimistic UPDATE alone cannot order two upstream
// PATCH side effects. The host-issued DB pool supplies the cross-node
// coordination; no global lock or plugin Redis connection is needed. Ark calls
// are already time bounded. If the connection/transaction is lost, its
// response cannot later commit over another node's index update. Upstream
// success followed by DB failure still requires a later refresh, as it does
// for every upstream-first operation.
func withResource[R any](db *gorm.DB, r indexResource[R], id int64,
	fn func(*gorm.DB, R) (*pluginv1.HTTPResponse, error),
) (*pluginv1.HTTPResponse, error) {
	var resp *pluginv1.HTTPResponse
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, lock := range r.locks {
			var locked int64
			// Row reports a lock that found no row as sql.ErrNoRows.
			if err := lock(tx, id).Row().Scan(&locked); err != nil {
				return err
			}
		}
		row, err := r.load(tx, id)
		if err != nil {
			return err
		}
		resp, err = fn(tx, row)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNoRow) {
		return notFound(r.missing), nil
	}
	return resp, err
}

// lockRow locks the row of m with the id: SELECT id ... FOR UPDATE.
func lockRow(m any) func(*gorm.DB, int64) *gorm.DB {
	return func(tx *gorm.DB, id int64) *gorm.DB {
		return tx.Model(m).Select("id").Where("id = ?", id).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
}

// groupResource locks an asset group row.
var groupResource = indexResource[*groupRow]{
	locks:   []func(*gorm.DB, int64) *gorm.DB{lockRow(&model.AssetGroup{})},
	load:    loadGroup,
	missing: "no such asset group in the index / 索引里没有这个素材组",
}

// assetResource locks an asset and, first, its parent group. Parent before
// child matches group deletion's cascade order; KEY SHARE prevents deleting
// the group without serializing distinct assets.
var assetResource = indexResource[*assetRow]{
	locks: []func(*gorm.DB, int64) *gorm.DB{
		func(tx *gorm.DB, id int64) *gorm.DB {
			return tx.Table("asset_groups g").Select("g.id").Joins("JOIN assets a ON a.group_id = g.id").
				Where("a.id = ?", id).Clauses(clause.Locking{Strength: "KEY SHARE", Table: clause.Table{Name: "g"}})
		},
		lockRow(&model.Asset{}),
	},
	load:    loadAsset,
	missing: "no such asset in the index / 索引里没有这个素材",
}
