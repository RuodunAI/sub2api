package gateway

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the sticky rule writes built with GORM (see package gormtest).
// The settings rows and the rule list stay on pgx (hotpath_bench_test.go).

func snapshotRule() *stickyRule {
	return &stickyRule{Name: "r", Source: sourceAdmin, Enabled: false, Priority: 0, OnFailure: onFailureStick,
		Match:      manifest.StickyMatch{Protocols: []string{"anthropic"}},
		KeySources: []manifest.StickyKeySource{{Type: "header", Name: "x-session"}}, TTLSeconds: 0}
}

// Whether a rule of the name is left, after a delete: one SELECT EXISTS.
func TestSQLStickyRuleNameInUse(t *testing.T) {
	var found bool
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return store.ExistsQuery(db, rulesNamed(db, "r")).Scan(&found)
	})
}

// A disabled rule of priority 0 and TTL 0 writes them (the columns default
// to true, 100 and 3600); updated_at takes its default, now().
func TestSQLStickyRuleCreate(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Create(adminRuleRow(snapshotRule(), 7)) })
}

// The update writes updated_at = now(), as the statement it replaces did.
func TestSQLStickyRuleUpdate(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return updateRule(db, 5, snapshotRule(), 0, true) })
}

// Restoring the default writes no updated_at.
func TestSQLStickyRuleUncustomize(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return uncustomizeRule(db, 5, 7) })
}

func TestSQLStickyRuleUpsertBuiltin(t *testing.T) {
	row := &model.StickyRule{Name: "b", Source: sourceBuiltin, Enabled: new(true), Priority: new(101),
		Match: mustJSON(manifest.StickyMatch{}), KeySources: mustJSON([]manifest.StickyKeySource{}),
		TTLSeconds: new(0), KeyIncludes: mustJSON(defaultKeyIncludes), OnFailure: new(onFailureFailover),
		PassHeaders: []string{}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return upsertBuiltinRule(db, row) })
}

func TestSQLStickyRuleDeleteStale(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return deleteStaleBuiltinRules(db, []string{"a", "b"}) })
}
