package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// JSON is a jsonb column or parameter decoded into a T, such as
// JSON[[]string] or JSON[map[string]int]. pgx decodes jsonb into Go values
// itself, but the database/sql path that GORM takes hands over the raw
// document; JSON does the json.Unmarshal that each caller would otherwise
// write. NULL reads as the zero T. The value is written as T's JSON, so a
// nil slice or map is the JSON null, not SQL NULL: a nullable column that
// should hold NULL takes a *JSON[T].
type JSON[T any] struct {
	V T `gorm:"-"` // GORM parses a Scan destination; V is no column of it
}

// Value implements driver.Valuer. The text form lets PostgreSQL type the
// parameter by its use (jsonb or json) instead of pgx sending bytea.
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
// nil is NULL and an empty non-nil slice the empty value, as pgx binds a
// []byte.
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

// Value implements driver.Valuer.
func (a Bools) Value() (driver.Value, error) {
	if a == nil {
		return []bool{}, nil
	}
	return []bool(a), nil
}

// GormDataType is that of Int64s.
func (Bools) GormDataType() string { return "boolean[]" }

// Int64s is a bigint[] parameter or column for GORM. GORM expands a plain
// []int64 argument into one placeholder per element, which breaks
// "= ANY(?)" and turns an empty slice into NULL; Int64s is one parameter
// that pgx encodes as an array, and nil is the empty array.
//
// It reads a column through database/sql (Scan: the slice the GORM bridge
// returns, pgx's []any, or the array's text form) and through pgx directly
// (the pgtype.ArraySetter methods, which pgx prefers to Scan, so it never
// hands Scan the binary wire form). A NULL array reads as nil; a NULL
// element is an error.
type Int64s []int64

// Value implements driver.Valuer.
func (a Int64s) Value() (driver.Value, error) {
	if a == nil {
		return []int64{}, nil
	}
	return []int64(a), nil
}

// GormDataType makes GORM map Int64s as a column: a slice type without it
// reads as a has-many relation and the model fails to parse.
func (Int64s) GormDataType() string { return "bigint[]" }

// Scan implements sql.Scanner.
func (a *Int64s) Scan(src any) error {
	return scanArray(src, pgtype.Int8ArrayOID, (*pgtype.FlatArray[int64])(a), func(e any) (int64, bool) {
		switch n := e.(type) {
		case int64:
			return n, true
		case int32:
			return int64(n), true
		case int16:
			return int64(n), true
		}
		return 0, false
	})
}

func (a *Int64s) SetDimensions(d []pgtype.ArrayDimension) error {
	return (*pgtype.FlatArray[int64])(a).SetDimensions(d)
}
func (a Int64s) ScanIndex(i int) any { return &a[i] }
func (Int64s) ScanIndexType() any    { return new(int64) }

// Strings is the text[] counterpart of Int64s.
type Strings []string

// Value implements driver.Valuer.
func (a Strings) Value() (driver.Value, error) {
	if a == nil {
		return []string{}, nil
	}
	return []string(a), nil
}

// GormDataType is that of Int64s.
func (Strings) GormDataType() string { return "text[]" }

// NullStrings is a as one text[] parameter, nil being SQL NULL: for SQL in
// which NULL means "no filter" (?::text[] IS NULL OR x = ANY(?)), where
// Strings(nil), the empty array, would match nothing.
func NullStrings(a []string) any {
	if a == nil {
		return nil
	}
	return Strings(a)
}

// Scan implements sql.Scanner.
func (a *Strings) Scan(src any) error {
	return scanArray(src, pgtype.TextArrayOID, (*pgtype.FlatArray[string])(a), func(e any) (string, bool) {
		s, ok := e.(string)
		return s, ok
	})
}

func (a *Strings) SetDimensions(d []pgtype.ArrayDimension) error {
	return (*pgtype.FlatArray[string])(a).SetDimensions(d)
}
func (a Strings) ScanIndex(i int) any { return &a[i] }
func (Strings) ScanIndexType() any    { return new(string) }

// Times is the timestamptz[] counterpart of Int64s.
type Times []time.Time

// Value implements driver.Valuer.
func (a Times) Value() (driver.Value, error) {
	if a == nil {
		return []time.Time{}, nil
	}
	return []time.Time(a), nil
}

// GormDataType is that of Int64s.
func (Times) GormDataType() string { return "timestamptz[]" }

// Scan implements sql.Scanner.
func (a *Times) Scan(src any) error {
	return scanArray(src, pgtype.TimestamptzArrayOID, (*pgtype.FlatArray[time.Time])(a), func(e any) (time.Time, bool) {
		t, ok := e.(time.Time)
		return t, ok
	})
}

func (a *Times) SetDimensions(d []pgtype.ArrayDimension) error {
	return (*pgtype.FlatArray[time.Time])(a).SetDimensions(d)
}
func (a Times) ScanIndex(i int) any { return &a[i] }
func (Times) ScanIndexType() any    { return new(time.Time) }

// scanArray stores src, a database/sql value of an array of E, into dst.
func scanArray[E any](src any, oid uint32, dst *pgtype.FlatArray[E], elem func(any) (E, bool)) error {
	switch v := src.(type) {
	case nil:
		*dst = nil
	case []E:
		*dst = v
	case []any:
		out := make([]E, len(v))
		for i, x := range v {
			e, ok := elem(x)
			if !ok {
				return fmt.Errorf("store: array element %d is %T, want %T", i, x, e)
			}
			out[i] = e
		}
		*dst = out
	case string:
		return scanArrayText(oid, []byte(v), dst)
	case []byte:
		return scanArrayText(oid, v, dst)
	default:
		return fmt.Errorf("store: cannot scan %T into %T", src, dst)
	}
	return nil
}

// scanArrayText parses the text form ("{1,2}"). A pgtype.Map is not safe
// for concurrent use, so each call takes its own; this is the uncommon
// path, the bridge and pgx hand over decoded arrays.
func scanArrayText[E any](oid uint32, src []byte, dst *pgtype.FlatArray[E]) error {
	return pgtype.NewMap().Scan(oid, pgtype.TextFormatCode, src, dst)
}
