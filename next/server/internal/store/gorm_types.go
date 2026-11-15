package store

import (
	"database/sql/driver"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

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
