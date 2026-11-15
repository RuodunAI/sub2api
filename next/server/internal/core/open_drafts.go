package core

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// MaxOpenDrafts is how many open account drafts one user may hold at a time
// (W1-F13): ccgateway draft runtimes and account preparations, each counted
// on its own. Callers with the full scope of the route are not limited.
const MaxOpenDrafts = 3

// LockOpenDrafts serializes, until tx ends, the count-and-insert of userID's
// open drafts: without it, concurrent requests each count the committed rows
// only and all pass the limit together (READ COMMITTED). Take it right
// before the counting insert.
func LockOpenDrafts(ctx context.Context, tx *gorm.DB, userID int64) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	return lockOpenDrafts(store.G(ctx, tx), userID).Error
}

func lockOpenDrafts(db *gorm.DB, userID int64) *gorm.DB {
	return db.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('open-drafts:' || ?::bigint::text, 0))`, userID)
}
