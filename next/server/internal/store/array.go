package store

import "github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"

// Array is one array parameter for GORM, which would expand a plain slice
// into a list, for the element types that Int64s and its kind do not cover:
// pgx encodes each element as it encodes a T parameter, so a nil pointer or
// a nil Bytes is a NULL element, and an Array[Bytes] cast to jsonb[] holds
// each document as its text. The SQL casts the parameter to its array type
// (?::jsonb[]), from which pgx takes the element type. Like Int64s, its
// Value is the plain slice and relies on pgx's database/sql driver taking
// it (TestArrayParametersRelyOnPgxStdlib). It is the array type the
// gateway and the plugin SDK use as well (gormpg.Array); its String shows a
// document as its text and a nil pointer or document as NULL (gormtest
// snapshots).
type Array[T any] = gormpg.Array[T]
