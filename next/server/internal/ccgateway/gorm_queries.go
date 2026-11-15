package ccgateway

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Query pieces shared by the statements built with GORM (store.G).

// runtimes is the ccgateway_runtimes table.
func runtimes(db *gorm.DB) *gorm.DB {
	return db.Model(&model.CcgatewayRuntime{})
}

// openDraft is the row of draft key while it is neither adopted nor
// retired.
func openDraft(db *gorm.DB, key string) *gorm.DB {
	return runtimes(db).Where("key = ? AND account_id IS NULL AND retired_at IS NULL", key)
}

// newAccountDraft is openDraft restricted to the drafts of new accounts (not
// re-authorization drafts) visible to scope (nil: every creator's).
func newAccountDraft(db *gorm.DB, key string, scope *int64) *gorm.DB {
	return openDraft(db, key).Where("for_account IS NULL").Scopes(createdBy(scope))
}

// createdBy keeps the drafts scope created; a nil scope keeps every draft.
func createdBy(scope *int64) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if scope == nil {
			return db
		}
		return db.Where("created_by = ?", *scope)
	}
}

// upsertAccountRow is the ON CONFLICT (account_id) of the per-account
// settings tables: the proposed row's cols replace the stored ones and
// updated_at moves to clock_timestamp().
func upsertAccountRow(cols ...string) clause.OnConflict {
	set := clause.AssignmentColumns(cols)
	set = append(set, clause.Assignment{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("clock_timestamp()")})
	return clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: set}
}
