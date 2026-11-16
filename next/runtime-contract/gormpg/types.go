package gormpg

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
)

// Array is a PostgreSQL array (text[], bigint[], jsonb[] ...) as one query
// parameter or a column. GORM expands a plain slice parameter into a list,
// one placeholder per element, and cannot scan an array column into one; an
// Array goes to pgx's driver as one array value and scans back from one.
// pgx encodes each element as it encodes a T parameter, so a nil pointer or
// a nil byte slice is a NULL element; the SQL may cast the parameter to its
// array type (?::jsonb[]), from which pgx takes the element type.
type Array[T any] []T

// Value hands the slice to the driver, which encodes it as an array; nil is
// NULL. Being a driver.Valuer is what keeps GORM from expanding it.
func (a Array[T]) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	return []T(a), nil
}

// Scan parses an array column, which database/sql hands over as its text
// (in DateStyle ISO, which ParseConfig pins); NULL is nil.
func (a *Array[T]) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	m := typeMaps.Get().(*pgtype.Map)
	defer typeMaps.Put(m)
	return m.SQLScanner((*[]T)(a)).Scan(src)
}

// GormDataType makes an Array field without a type tag acceptable to GORM,
// e.g. in a query result. A model column still names its type
// (`gorm:"type:text[]"`).
func (Array[T]) GormDataType() string { return "array" }

// String shows the values, a byte slice element (a document) as its text
// and a nil pointer or byte slice as NULL, for SQL snapshots.
func (a Array[T]) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(' ')
		}
		writeElement(&b, reflect.ValueOf(&v).Elem())
	}
	b.WriteByte(']')
	return b.String()
}

func writeElement(b *strings.Builder, v reflect.Value) {
	switch {
	case v.Kind() == reflect.Interface && !v.IsNil():
		writeElement(b, v.Elem())
	case (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil():
		b.WriteString("NULL")
	case v.Kind() == reflect.Pointer:
		writeElement(b, v.Elem())
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		if v.IsNil() {
			b.WriteString("NULL")
		} else {
			b.Write(v.Bytes())
		}
	default:
		fmt.Fprint(b, v.Interface())
	}
}

// typeMaps parse array values. A pgtype.Map caches scan plans without a
// lock, so concurrent scans take one each; a pool is cheaper than a new map
// per scan.
var typeMaps = sync.Pool{New: func() any { return pgtype.NewMap() }}
