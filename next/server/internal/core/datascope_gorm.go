package core

import (
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Scope is Cond and Args as a GORM scope: the same fixed text, so the plan
// is shared with every other user of Cond, and the id lists as store.Int64s, each
// one array parameter (GORM would expand a plain []int64).
func (t ScopeTable) Scope(s DataScope) func(*gorm.DB) *gorm.DB {
	cond, args := t.Cond(), t.Args(s)
	for i, a := range args {
		if ids, ok := a.([]int64); ok {
			args[i] = store.Int64s(ids)
		}
	}
	return func(db *gorm.DB) *gorm.DB { return db.Where(cond, args...) }
}
