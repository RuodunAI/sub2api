package pgdb

import (
	"fmt"
	"reflect"
	"testing"
)

func TestBatchRowsStaysUnderTheParameterLimit(t *testing.T) {
	// Open does no I/O: the handle only parses models here.
	db, err := Open("postgres://u@127.0.0.1:1/db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db) //nolint:errcheck
	type narrow struct {
		ID   int64
		Name string
	}
	if n := batchRows(db, new(narrow)); n != MaxBatchRows {
		t.Errorf("narrow model: %d rows per batch, want %d", n, MaxBatchRows)
	}
	// 100 columns: 655 rows stay under 65535 parameters.
	fields := []reflect.StructField{{Name: "ID", Type: reflect.TypeFor[int64]()}}
	for i := range 99 {
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("C%d", i), Type: reflect.TypeFor[int]()})
	}
	wide := reflect.New(reflect.StructOf(fields)).Interface()
	if n := batchRows(db, wide); n != 655 {
		t.Errorf("100 columns: %d rows per batch, want 655", n)
	}
}
