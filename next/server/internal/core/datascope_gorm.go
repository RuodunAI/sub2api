package core

import (
	"database/sql/driver"

	"gorm.io/gorm"
)

// Scope is Cond and Args as a GORM scope: the same fixed text, so the plan
// is shared with every other user of Cond, and each id list one array
// parameter (int64s; GORM would expand a plain []int64).
func (t ScopeTable) Scope(s DataScope) func(*gorm.DB) *gorm.DB {
	cond, args := t.Cond(), t.Args(s)
	for i, a := range args {
		if ids, ok := a.([]int64); ok {
			args[i] = int64s(ids)
		}
	}
	return func(db *gorm.DB) *gorm.DB { return db.Where(cond, args...) }
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
