package store

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
)

// Array is one array parameter for GORM, which would expand a plain slice
// into a list, for the element types that Int64s and its kind do not cover:
// pgx encodes each element as it encodes a T parameter, so a nil pointer or
// a nil Bytes is a NULL element, and an Array[Bytes] cast to jsonb[] holds
// each document as its text. The SQL casts the parameter to its array type
// (?::jsonb[]), from which pgx takes the element type.
type Array[T any] []T

// Value implements driver.Valuer.
func (a Array[T]) Value() (driver.Value, error) { return []T(a), nil }

// String shows the values, a document as its text and a nil pointer or
// document as NULL (gormtest snapshots).
func (a Array[T]) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(' ')
		}
		writeElement(&b, v)
	}
	b.WriteByte(']')
	return b.String()
}

func writeElement(b *strings.Builder, v any) {
	if doc, ok := v.(Bytes); ok {
		if doc == nil {
			b.WriteString("NULL")
		} else {
			b.Write(doc)
		}
		return
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			b.WriteString("NULL")
			return
		}
		v = rv.Elem().Interface()
	}
	fmt.Fprint(b, v)
}
