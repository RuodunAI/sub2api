package fallbackcredits

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the fallback credit statements built with GORM (see package
// gormtest).

var snapshotOwner = core.ResourceOwner{UserID: 7, GroupID: 3}

func TestSnapshotCreditPurgeExpired(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return ownerCredits(db, snapshotOwner).Where("expires_at < now()-interval '24 hours'").Delete(&model.ProviderFallbackCredit{})
	})
}

func TestSnapshotCreditRetained(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var n int64
		return ownerCredits(db, snapshotOwner).Count(&n)
	})
}

func TestSnapshotCreditRecord(t *testing.T) {
	at := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	in := core.FallbackCredit{TokenHash: "t", PluginKey: "p", SourceModel: "m", Owner: snapshotOwner,
		Binding:       core.ResourceBinding{AccountID: 9, PrincipalID: "pr", Generation: "g1"},
		PromptDigests: []string{"d1", "d2"}, ObservedAt: at, ExpiresAt: at.Add(5 * time.Minute)}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(newRow(in))
	})
}

func TestSnapshotCreditRead(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var r model.ProviderFallbackCredit
		return db.Take(&r, "token_hash = ?", "t")
	})
}
