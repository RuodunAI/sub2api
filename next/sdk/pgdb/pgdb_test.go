package pgdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// tagged has defaults GORM cannot compare as text: an ARRAY constructor
// reads ARRAY['a' to it, cut at the first "::".
type tagged struct {
	ID    int64              `gorm:"primaryKey"`
	Tags  pgdb.Array[string] `gorm:"type:text[];not null;default:ARRAY['a','b']"`
	Kind  string             `gorm:"type:varchar(8);not null;default:'x'"`
	Score int64              `gorm:"not null;default:0"`
}

func (tagged) TableName() string { return "tagged" }

const taggedSQL = `CREATE TABLE tagged (
    id    bigserial  PRIMARY KEY,
    tags  text[]     NOT NULL DEFAULT ARRAY['a', 'b'],
    kind  varchar(8) NOT NULL DEFAULT 'x'::character varying,
    score bigint     NOT NULL DEFAULT 0
);`

func TestDriftComparesDefaultsByMeaning(t *testing.T) {
	if r := drift(t, migrate(t, taggedSQL), &tagged{}); !r.Clean() {
		t.Fatalf("same defaults: %+v", r)
	}
	r := drift(t, migrate(t, strings.Replace(taggedSQL, "ARRAY['a', 'b']", "ARRAY['a']", 1)), &tagged{})
	if !slices.Equal(r.Statements, []string{`ALTER TABLE "tagged" ALTER COLUMN "tags" SET DEFAULT ARRAY['a','b']`}) || len(r.Unmapped) != 0 {
		t.Fatalf("changed default: %+v", r)
	}
}

// child maps the columns of the table below, none of its constraints.
type child struct {
	ID     int64 `gorm:"primaryKey;autoIncrement:false"`
	ItemID *int64
	Code   *string
}

func (child) TableName() string { return "child" }

const childSQL = `CREATE TABLE child (
    id      bigint PRIMARY KEY,
    item_id bigint CONSTRAINT child_item_id_fkey REFERENCES item (id),
    code    text UNIQUE,
    CHECK (id > 0)
);`

func TestDriftReportsUnmappedObjects(t *testing.T) {
	r := drift(t, migrate(t, itemSQL, childSQL, `
ALTER TABLE item ADD CONSTRAINT item_name_len CHECK (length(name) < 100);
CREATE INDEX item_lower_name_idx ON item (lower(name));
CREATE VIEW item_names AS SELECT name FROM item;
CREATE SEQUENCE item_seq;
CREATE FUNCTION item_count(least_id bigint) RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM item WHERE id >= least_id';`),
		&item{}, &child{})
	want := []string{
		"constraint child.child_code_key UNIQUE (code)",
		"constraint child.child_id_check CHECK ((id > 0))",
		"constraint child.child_item_id_fkey FOREIGN KEY (item_id) REFERENCES item(id)",
		"constraint item.item_name_len CHECK ((length(name) < 100))",
		"function item_count(least_id bigint)",
		"index item.item_lower_name_idx",
		"sequence item_seq",
		"view item_names",
	}
	if len(r.Statements) != 0 || !slices.Equal(r.Unmapped, want) {
		t.Fatalf("drift = %+v", r)
	}
}

// mappedChild maps them all.
type mappedChild struct {
	ID     int64   `gorm:"primaryKey;autoIncrement:false;check:child_id_check,id > 0"`
	ItemID *int64  `gorm:"index:child_item_idx"`
	Code   *string `gorm:"unique"`
	Item   *item   `gorm:"foreignKey:ItemID;constraint:child_item_id_fkey,"`
}

func (mappedChild) TableName() string { return "child" }

func TestDriftMapsConstraintsByName(t *testing.T) {
	r := drift(t, migrate(t, itemSQL, childSQL, `CREATE INDEX child_item_idx ON child (item_id);`), &item{}, &mappedChild{})
	if !r.Clean() {
		t.Fatalf("drift = %+v", r)
	}
}

// A model that drops `unique` from a field whose constraint the migration
// named <table>_<column>_key: GORM would drop uni_child_code, which does not
// exist. The constraint is reported instead, by its name.
func TestDriftReportsDroppedUnique(t *testing.T) {
	r := drift(t, migrate(t, `CREATE TABLE child (id bigint PRIMARY KEY, item_id bigint, code text UNIQUE)`), &child{})
	if len(r.Statements) != 0 || !slices.Equal(r.Unmapped, []string{"constraint child.child_code_key UNIQUE (code)"}) {
		t.Fatalf("drift = %+v", r)
	}
}

// pgxpool's pool_* parameters are no PostgreSQL settings: Open drops them,
// in either DSN form.
func TestOpenIgnoresPoolParams(t *testing.T) {
	u, err := url.Parse(testpg.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	q := u.Query()
	q.Set("pool_max_conns", "5")
	q.Set("pool_max_conn_lifetime", "1h")
	withQuery := *u
	withQuery.RawQuery = q.Encode()
	kv := fmt.Sprintf("host=%s port=%s user=%s password='%s' dbname=%s sslmode=disable pool_max_conns=5 pool_min_conns=1",
		u.Hostname(), u.Port(), u.User.Username(), pass, strings.TrimPrefix(u.Path, "/"))
	for _, dsn := range []string{withQuery.String(), kv} {
		db, err := pgdb.Open(dsn, pgdb.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var one int
		err = db.Raw("SELECT 1").Scan(&one).Error
		pgdb.Close(db) //nolint:errcheck
		if err != nil || one != 1 {
			t.Fatalf("query through a DSN with pool parameters: %v", err)
		}
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
