package store

import "database/sql/driver"

// Int64s is a bigint[] parameter or column for GORM. GORM expands a plain
// []int64 argument into one placeholder per element, which breaks
// "= ANY(?)" and turns an empty slice into NULL; Int64s is one parameter
// that pgx encodes as an array, and nil is the empty array.
type Int64s []int64

// Value implements driver.Valuer.
func (a Int64s) Value() (driver.Value, error) {
	if a == nil {
		return []int64{}, nil
	}
	return []int64(a), nil
}

// Strings is the text[] counterpart of Int64s.
type Strings []string

// Value implements driver.Valuer.
func (a Strings) Value() (driver.Value, error) {
	if a == nil {
		return []string{}, nil
	}
	return []string(a), nil
}
