package pgdb_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

// A constraint violation is both GORM's error and the driver's.
func TestErrorsTranslated(t *testing.T) {
	// One connection: the temporary table lives on it.
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pgdb.Close(db) //nolint:errcheck
	if err := db.Exec(`CREATE TEMPORARY TABLE uniq (id int PRIMARY KEY)`).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err = db.Exec(`INSERT INTO uniq VALUES (1)`).Error
	}
	if !errors.Is(err, gorm.ErrDuplicatedKey) || !strings.Contains(err.Error(), "uniq_pkey") {
		t.Fatalf("duplicate insert: %v", err)
	}
}

func TestArrayRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer pgdb.Close(db) //nolint:errcheck
	var got struct {
		Tags pgdb.Array[string]
		IDs  pgdb.Array[int64]
		Hit  bool
		Nil  pgdb.Array[string]
	}
	in := pgdb.Array[string]{"a", "b c", `"q"`}
	err = db.WithContext(ctx).Raw(`SELECT ?::text[] AS tags, ?::bigint[] AS ids, 2 = ANY(?) AS hit, NULL::text[] AS nil`,
		in, pgdb.Array[int64]{1, 2}, pgdb.Array[int64]{2}).Scan(&got).Error
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Tags, in) || !slices.Equal(got.IDs, pgdb.Array[int64]{1, 2}) || !got.Hit || got.Nil != nil {
		t.Fatalf("got %+v", got)
	}
}

// Array scans run concurrently (go test -race).
func TestArrayScanConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 200 {
				var a pgdb.Array[int64]
				if err := a.Scan(fmt.Sprintf("{%d,2}", i)); err != nil || len(a) != 2 || a[0] != int64(i) {
					t.Errorf("scan: %v %v", a, err)
					return
				}
				var s pgdb.Array[string]
				if err := s.Scan([]byte(`{"a b",c}`)); err != nil || len(s) != 2 {
					t.Errorf("scan: %v %v", s, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// IsUniqueViolation holds where GORM translates errors and where it does not.
func TestIsUniqueViolation(t *testing.T) {
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pgdb.Close(db) //nolint:errcheck
	if err := db.Exec(`CREATE TEMPORARY TABLE uniq2 (id int PRIMARY KEY)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO uniq2 VALUES (1)`).Error; err != nil {
		t.Fatal(err)
	}
	execErr := db.Exec(`INSERT INTO uniq2 VALUES (1)`).Error
	var id int
	rowErr := db.Raw(`INSERT INTO uniq2 VALUES (1) RETURNING id`).Row().Scan(&id)
	if !pgdb.IsUniqueViolation(execErr) || !pgdb.IsUniqueViolation(rowErr) {
		t.Fatalf("exec: %v, row: %v", execErr, rowErr)
	}
	if errors.Is(rowErr, gorm.ErrDuplicatedKey) {
		t.Fatal("GORM translates Row errors now: update the Database section of package pluginsdk")
	}
	if pgdb.IsUniqueViolation(errors.New("23505")) || pgdb.IsUniqueViolation(nil) {
		t.Fatal("not a unique violation")
	}
}
