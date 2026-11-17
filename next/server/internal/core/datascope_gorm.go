package core

import (
	"database/sql/driver"
	"slices"

	"gorm.io/gorm"
)

// Scope is the predicate of t for s as a GORM scope: the same fixed text
// for every scope, so the plan is shared, each id list one array parameter
// (int64s; GORM would expand a plain []int64), and the users below the
// holders of a team (s.Teams) a subquery on UserTree's index.
func (t ScopeTable) Scope(s DataScope) func(*gorm.DB) *gorm.DB {
	anyGroup := slices.Contains(s.Groups, 0)
	groups := slices.DeleteFunc(slices.Clone(s.Groups), func(g int64) bool { return g == 0 })
	return func(db *gorm.DB) *gorm.DB {
		args := []any{s.All, int64s(s.Owners), UserTree().Below(db, s.Teams...), int64s(s.IDs)}
		if t.grouped {
			args = append(args, anyGroup, int64s(groups), int64s(s.PairG), int64s(s.PairID))
		}
		return db.Where(t.cond(), args...)
	}
}

// int64s is an id list as one bigint[] parameter, nil being the empty
// array: GORM binds a driver.Valuer as is, and pgx's database/sql driver
// encodes the plain slice Value returns (as store.Int64s does, which core
// does not import: store is an implementation of core's ports).
type int64s []int64

// Value implements driver.Valuer.
func (a int64s) Value() (driver.Value, error) {
	if a == nil {
		return []int64{}, nil
	}
	return []int64(a), nil
}
