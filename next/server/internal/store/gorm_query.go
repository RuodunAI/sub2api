package store

import "gorm.io/gorm"

// Query building helpers for the GORM path.

// Conds are WHERE conditions gathered one at a time and applied together:
// the GORM counterpart of Filter. The zero value holds none.
type Conds []func(*gorm.DB) *gorm.DB

// And adds cond, whose "?" placeholders take args in order; the conditions
// are ANDed. An array argument is an Int64s, Strings or Times, as for Where.
func (c *Conds) And(cond string, args ...any) {
	*c = append(*c, func(db *gorm.DB) *gorm.DB { return db.Where(cond, args...) })
}

// Scope applies the conditions, for db.Scopes.
func (c Conds) Scope() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		for _, cond := range c {
			db = cond(db)
		}
		return db
	}
}

// SetIf puts *v into set under column unless v is nil: set is the column map
// of a partial UPDATE, in which a nil field is one the request left out and
// the column keeps its value.
func SetIf[T any](set map[string]any, column string, v *T) {
	if v != nil {
		set[column] = *v
	}
}

// The row lock strengths that gorm's clause package has no constant for
// (it has clause.LockingStrengthUpdate and clause.LockingStrengthShare).
const (
	LockingStrengthNoKeyUpdate = "NO KEY UPDATE"
	LockingStrengthKeyShare    = "KEY SHARE"
)
