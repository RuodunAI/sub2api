package pgdb_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

// entry is a row of the temporary table tempDB creates.
type entry struct {
	ID   int64 `gorm:"primaryKey"`
	Seq  int
	Name string
}

func (entry) TableName() string { return "entry" }

// tempDB is a handle on one connection, where the temporary table entry
// lives: name is unique.
func tempDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pgdb.Close(db) })
	if err := db.Exec(`CREATE TEMPORARY TABLE entry (id bigserial PRIMARY KEY, seq int NOT NULL, name text NOT NULL UNIQUE)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func entries(n int) []entry {
	out := make([]entry, n)
	for i := range out {
		out[i] = entry{Seq: i, Name: fmt.Sprintf("e%05d", i)}
	}
	return out
}

func seqs(t *testing.T, db *gorm.DB) []int {
	t.Helper()
	var got []int
	if err := db.Model(&entry{}).Order("id").Pluck("seq", &got).Error; err != nil {
		t.Fatal(err)
	}
	return got
}

// Insert writes several batches in the order given, with the clauses on
// each of them.
func TestInsertKeepsOrderAcrossBatches(t *testing.T) {
	db := tempDB(t)
	rows := entries(2*pgdb.MaxBatchRows + 5)
	slices.Reverse(rows)
	if err := pgdb.Insert(db, rows); err != nil {
		t.Fatal(err)
	}
	got := seqs(t, db)
	want := make([]int, len(rows))
	for i, r := range rows {
		want[i] = r.Seq
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows not inserted in the order given (%d rows)", len(got))
	}
	// ON CONFLICT on every batch: nothing new, no error.
	if err := pgdb.Insert(db, rows, clause.OnConflict{DoNothing: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(seqs(t, db)); n != len(rows) {
		t.Fatalf("%d rows after the conflicting insert, want %d", n, len(rows))
	}
	if err := pgdb.Insert(db, []entry(nil)); err != nil {
		t.Fatalf("empty insert: %v", err)
	}
}

// InsertAll writes all batches or none.
func TestInsertAllIsAtomic(t *testing.T) {
	db := tempDB(t)
	rows := entries(2*pgdb.MaxBatchRows + 5)
	rows[len(rows)-1].Name = rows[0].Name // fails in the last batch
	if err := pgdb.InsertAll(db, rows); !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("insert = %v, want a duplicate key", err)
	}
	if n := len(seqs(t, db)); n != 0 {
		t.Fatalf("%d rows left by a failed InsertAll", n)
	}
	if err := pgdb.InsertAll(db, entries(3)); err != nil {
		t.Fatal(err)
	}
	if got := seqs(t, db); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("rows = %v", got)
	}
}

func TestList(t *testing.T) {
	db := tempDB(t)
	if err := pgdb.Insert(db, entries(7)); err != nil {
		t.Fatal(err)
	}
	type item struct {
		Seq int
	}
	q := db.Model(&entry{}).Where("seq >= ?", 1)
	rows, total, err := pgdb.List[item](q, "seq", "seq DESC", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 || !slices.Equal(rows, []item{{4}, {3}}) {
		t.Fatalf("page = %v of %d, want [{4} {3}] of 6", rows, total)
	}
	// Past the end: no rows (an empty slice), still the total.
	rows, total, err = pgdb.List[item](q, "seq", "seq", 2, 10)
	if err != nil || rows == nil || len(rows) != 0 || total != 6 {
		t.Fatalf("empty page = %#v of %d (%v)", rows, total, err)
	}
	// One column, read into its type.
	names, _, err := pgdb.List[json.RawMessage](q, "to_jsonb(name)", "seq", 1, 0)
	if err != nil || len(names) != 1 || string(names[0]) != `"e00001"` {
		t.Fatalf("one column = %q (%v)", names, err)
	}

	var le *pgdb.ListError
	_, _, err = pgdb.List[item](db.Table("missing"), "seq", "seq", 2, 0)
	if !errors.As(err, &le) || !le.Count || err.Error() != le.Err.Error() {
		t.Fatalf("count failure = %#v", err)
	}
	_, _, err = pgdb.List[item](db.Model(&entry{}), "nope", "seq", 2, 0)
	if !errors.As(err, &le) || le.Count {
		t.Fatalf("page failure = %#v", err)
	}
}

// On a DriverErrors handle a translated error has the driver's text and is
// still the gorm error; the handle it was made from is left alone.
func TestDriverErrors(t *testing.T) {
	base := tempDB(t)
	db := pgdb.DriverErrors(base)
	if pgdb.DriverErrors(db) != db {
		t.Error("DriverErrors of its own handle made another one")
	}
	if err := db.Create(&entry{Seq: 1, Name: "dup"}).Error; err != nil {
		t.Fatal(err)
	}
	translated := base.Create(&entry{Seq: 2, Name: "dup"}).Error
	driver, ok := strings.CutPrefix(translated.Error(), gorm.ErrDuplicatedKey.Error()+": ")
	if !ok || !strings.HasPrefix(driver, "ERROR: duplicate key value violates unique constraint") {
		t.Fatalf("base handle: %v", translated)
	}
	for name, err := range map[string]error{
		"create": db.Create(&entry{Seq: 2, Name: "dup"}).Error,
		"exec":   db.Exec(`INSERT INTO entry (seq, name) VALUES (2, 'dup')`).Error,
		// A transaction and a savepoint are made from the handle too.
		"nested": db.Transaction(func(tx *gorm.DB) error {
			return tx.Transaction(func(tx *gorm.DB) error { return tx.Create(&entry{Seq: 2, Name: "dup"}).Error })
		}),
	} {
		if !errors.Is(err, gorm.ErrDuplicatedKey) || err.Error() != driver {
			t.Errorf("%s: %v, want the driver's text %q", name, err, driver)
		}
	}
	check := db.Exec(`ALTER TABLE entry ADD CONSTRAINT entry_seq_small CHECK (seq < 10)`).Error
	if check != nil {
		t.Fatal(check)
	}
	err := fmt.Errorf("update: %w", db.Model(&entry{}).Where("seq = 1").Update("seq", 11).Error)
	if !errors.Is(err, gorm.ErrCheckConstraintViolated) || !strings.HasPrefix(err.Error(), "update: ERROR: new row for relation") {
		t.Errorf("check violation: %v", err)
	}
	if err := db.Exec(`SELECT 1`).Error; err != nil {
		t.Fatal(err)
	}
}
