package pgdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

// item is the model the migration of migrate() creates, exactly.
type item struct {
	ID      int64              `gorm:"primaryKey"`
	Name    string             `gorm:"not null;default:'';index:item_name_idx"`
	Tags    pgdb.Array[string] `gorm:"type:text[];not null;default:'{}'"`
	Doc     json.RawMessage    `gorm:"type:jsonb;not null;default:'{}'"`
	Until   time.Time          `gorm:"not null;default:'-infinity'"`
	Created time.Time          `gorm:"not null;default:now()"`
}

func (item) TableName() string { return "item" }

const itemSQL = `CREATE TABLE item (
    id      bigserial   PRIMARY KEY,
    name    text        NOT NULL DEFAULT '',
    tags    text[]      NOT NULL DEFAULT '{}',
    doc     jsonb       NOT NULL DEFAULT '{}',
    until   timestamptz NOT NULL DEFAULT '-infinity',
    created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX item_name_idx ON item (name);`

// migrate writes the migrations and returns them as Drift reads them.
func migrate(t *testing.T, files ...string) []pgdb.Migration {
	t.Helper()
	dir := t.TempDir()
	for i, f := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d_m.sql", i+1)), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := pgdb.ReadMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func drift(t *testing.T, migrations []pgdb.Migration, models ...any) *pgdb.DriftReport {
	t.Helper()
	r, err := pgdb.Drift(context.Background(), testpg.URL(t), migrations, models)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDriftCleanWhenModelsMatch(t *testing.T) {
	if r := drift(t, migrate(t, itemSQL), &item{}); !r.Clean() {
		t.Fatalf("drift = %+v", r)
	}
}

func TestDriftCreatesWhatIsMissing(t *testing.T) {
	r := drift(t, migrate(t), &item{})
	if len(r.Statements) != 2 || !strings.HasPrefix(r.Statements[0], `CREATE TABLE "item"`) ||
		!strings.Contains(r.Statements[1], `"item_name_idx"`) || len(r.Unmapped) != 0 {
		t.Fatalf("drift = %+v", r)
	}
}

// itemV2 adds a column and an index, drops tags and widens nothing.
type itemV2 struct {
	ID      int64           `gorm:"primaryKey"`
	Name    string          `gorm:"not null;default:'';index:item_name_idx"`
	Doc     json.RawMessage `gorm:"type:jsonb;not null;default:'{}'"`
	Until   time.Time       `gorm:"not null;default:'-infinity'"`
	Created time.Time       `gorm:"not null;default:now();index:item_created_idx"`
	Score   int64           `gorm:"not null;default:0"`
}

func (itemV2) TableName() string { return "item" }

func TestDriftReportsAdditionsAndUnmapped(t *testing.T) {
	r := drift(t, migrate(t, itemSQL, `CREATE TABLE extra (id int); CREATE INDEX item_old_idx ON item (until);`), &itemV2{})
	want := []string{
		`ALTER TABLE "item" ADD "score" bigint NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS "item_created_idx" ON "item" ("created")`,
	}
	if !slices.Equal(r.Statements, want) {
		t.Errorf("statements = %q", r.Statements)
	}
	if want := []string{"column item.tags", "index item.item_old_idx", "table extra"}; !slices.Equal(r.Unmapped, want) {
		t.Errorf("unmapped = %q", r.Unmapped)
	}
}

// itemText changes the type of name.
type itemText struct {
	ID      int64              `gorm:"primaryKey"`
	Name    string             `gorm:"type:varchar(20);not null;default:'';index:item_name_idx"`
	Tags    pgdb.Array[string] `gorm:"type:text[];not null;default:'{}'"`
	Doc     json.RawMessage    `gorm:"type:jsonb;not null;default:'{}'"`
	Until   time.Time          `gorm:"not null;default:'-infinity'"`
	Created time.Time          `gorm:"not null;default:now()"`
}

func (itemText) TableName() string { return "item" }

func TestDriftReportsTypeChanges(t *testing.T) {
	r := drift(t, migrate(t, itemSQL), &itemText{})
	if len(r.Statements) != 1 || !strings.Contains(r.Statements[0], `ALTER COLUMN "name" TYPE varchar(20)`) {
		t.Fatalf("statements = %q", r.Statements)
	}
}

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
