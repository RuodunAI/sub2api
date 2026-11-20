package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// JSON is a jsonb column or parameter decoded into a T, such as
// JSON[[]string] or JSON[map[string]int]. database/sql hands over the raw
// document; JSON does the json.Unmarshal that each caller would otherwise
// write. NULL reads as the zero T. The value is written as T's JSON, so a
// nil slice or map is the JSON null, not SQL NULL: a nullable column that
// should hold NULL takes a *JSON[T].
type JSON[T any] struct {
	V T `gorm:"-"` // GORM parses a Scan destination; V is no column of it
}

// Value implements driver.Valuer. The text form lets PostgreSQL type the
// parameter by its use (jsonb or json) instead of the driver sending bytea.
func (j JSON[T]) Value() (driver.Value, error) {
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, fmt.Errorf("store: encode JSON[%T]: %w", j.V, err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (j *JSON[T]) Scan(src any) error {
	var zero T
	j.V = zero
	var doc []byte
	switch v := src.(type) {
	case nil:
		return nil
	case []byte:
		doc = v
	case string:
		doc = []byte(v)
	default:
		return fmt.Errorf("store: cannot scan %T into %T", src, j)
	}
	if err := json.Unmarshal(doc, &j.V); err != nil {
		return fmt.Errorf("store: decode %T: %w", j, err)
	}
	return nil
}

// GormDataType makes GORM map JSON as a column rather than a relation.
func (JSON[T]) GormDataType() string { return "jsonb" }

// MarshalJSON and UnmarshalJSON make JSON[T] encode as its T, so a model
// that carries one serializes without a {"V": ...} wrapper.
func (j JSON[T]) MarshalJSON() ([]byte, error)  { return json.Marshal(j.V) }
func (j *JSON[T]) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &j.V) }

// Bytes is a bytea (or json/jsonb document) parameter for GORM that stays
// one parameter wherever it stands. GORM spreads a []byte or
// json.RawMessage written right after "(" (COALESCE(?, ...)) into one
// placeholder per byte and turns an empty one into NULL there, and it
// turns an empty json.RawMessage into NULL anywhere. Bytes is bound as is:
// nil is NULL and an empty non-nil slice the empty value, as the driver
// binds a []byte.
type Bytes []byte

// Value implements driver.Valuer.
func (b Bytes) Value() (driver.Value, error) {
	if b == nil {
		return nil, nil
	}
	return []byte(b), nil
}

// Scan implements sql.Scanner: NULL reads as nil. The bytes are copied,
// since database/sql may reuse the source buffer.
func (b *Bytes) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*b = nil
	case []byte:
		*b = append(Bytes{}, v...)
	case string:
		*b = Bytes(v)
	default:
		return fmt.Errorf("store: cannot scan %T into %T", src, b)
	}
	return nil
}

// GormDataType makes GORM map Bytes as a column.
func (Bytes) GormDataType() string { return "bytea" }

// Bools is a boolean[] parameter for GORM, which expands a plain []bool
// into a list (see Int64s); nil is the empty array.
type Bools []bool

// Value implements driver.Valuer with the plain slice (see Int64s).
func (a Bools) Value() (driver.Value, error) { return orEmpty([]bool(a)), nil }

// GormDataType is that of Int64s.
func (Bools) GormDataType() string { return "boolean[]" }

// Int64s is a bigint[] parameter or column for GORM. GORM expands a plain
// []int64 argument into one placeholder per element, which breaks
// "= ANY(?)" and turns an empty slice into NULL; Int64s is one parameter
// (GORM binds a driver.Valuer as is), and nil is the empty array.
//
// Value returns the plain []int64, which is no driver.Value of
// database/sql's: the parameter relies on pgx's database/sql driver, whose
// CheckNamedValue accepts any argument and hands it to pgx, which calls
// Value and encodes the slice as the parameter's array type, in binary, as
// it encodes a []int64. (Writing the array's text instead made pgx parse
// it again: up to 1.7 times the allocations of a statement with array
// parameters.) Strings, Times, Bools and Array do the same;
// TestArrayParametersRelyOnPgxStdlib keeps it.
//
// It reads a column from the array's text form, which database/sql gives
// every array. A NULL array reads as nil, the empty one as an empty slice;
// a NULL element is an error.
type Int64s []int64

// Value implements driver.Valuer with the plain slice (see above).
func (a Int64s) Value() (driver.Value, error) { return orEmpty([]int64(a)), nil }

// orEmpty is a, or an empty slice for nil, which pgx would bind as NULL.
func orEmpty[T any](a []T) []T {
	if a == nil {
		return []T{}
	}
	return a
}

// GormDataType makes GORM map Int64s as a column: a slice type without it
// reads as a has-many relation and the model fails to parse.
func (Int64s) GormDataType() string { return "bigint[]" }

// Scan implements sql.Scanner.
func (a *Int64s) Scan(src any) (err error) {
	*a, err = scanArray(src, func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) })
	return err
}

// Strings is the text[] counterpart of Int64s.
type Strings []string

// Value implements driver.Valuer with the plain slice (see Int64s).
func (a Strings) Value() (driver.Value, error) { return orEmpty([]string(a)), nil }

// GormDataType is that of Int64s.
func (Strings) GormDataType() string { return "text[]" }

// NullStrings is a as one text[] parameter, nil being SQL NULL: for SQL in
// which NULL means "no filter" (?::text[] IS NULL OR x = ANY(?)), where
// Strings(nil), the empty array, would match nothing.
func NullStrings(a []string) any {
	if p := NullStringsPtr(a); p != nil {
		return *p
	}
	return nil
}

// NullStringsPtr is a as a nullable text[] value, such as a *Strings model
// field: nil (SQL NULL) for a nil slice.
func NullStringsPtr(a []string) *Strings {
	if a == nil {
		return nil
	}
	return new(Strings(a))
}

// Scan implements sql.Scanner.
func (a *Strings) Scan(src any) (err error) {
	*a, err = scanArray(src, func(s string) (string, error) { return s, nil })
	return err
}

// Times is the timestamptz[] counterpart of Int64s. Its elements read in
// time.Local; ±infinity is an error, as no time.Time holds it.
type Times []time.Time

// Value implements driver.Valuer with the plain slice (see Int64s); pgx
// drops the sub-microsecond part.
func (a Times) Value() (driver.Value, error) { return orEmpty([]time.Time(a)), nil }

// GormDataType is that of Int64s.
func (Times) GormDataType() string { return "timestamptz[]" }

// Scan implements sql.Scanner.
func (a *Times) Scan(src any) (err error) {
	*a, err = scanArray(src, parseTimestamptz)
	return err
}
