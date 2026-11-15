// Package gormtest snapshots the SQL that GORM builds, without a database.
//
// GORM only builds statements in this codebase; pgx executes them (see the
// GORM notes in package store). What a migrated repository sends to
// PostgreSQL is therefore fully decided by the query builder, and a golden
// file of its SQL and arguments is the review surface for a migration: the
// diff shows exactly how a statement changed. Assert compares a query with
// testdata/<test name>.golden; `go test -run <test> -update` rewrites it.
package gormtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// update is registered by this package, so a test package that imports it
// gets -update without declaring it (and must not declare its own).
var update = flag.Bool("update", false, "rewrite the SQL snapshots under testdata")

var (
	dryOnce sync.Once
	dry     *gorm.DB
	dryErr  error
)

// errNoConnect is what a statement that escaped DryRun would get: the
// instance has no server behind it, by construction.
var errNoConnect = errors.New("gormtest: the DryRun instance never connects")

type refuseConnector struct{}

func (refuseConnector) Connect(context.Context) (driver.Conn, error) { return nil, errNoConnect }
func (refuseConnector) Driver() driver.Driver                        { return nil }

// DB returns a shared GORM instance in DryRun mode with the postgres
// dialect. It is independent of store.G, but its gorm.Config must stay in
// step with the one store.G uses (naming, default transaction off), or the
// snapshots would describe statements production never builds.
func DB() (*gorm.DB, error) {
	dryOnce.Do(func() {
		conn := sql.OpenDB(refuseConnector{})
		dry, dryErr = gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{
			DryRun:                 true,
			DisableAutomaticPing:   true,
			SkipDefaultTransaction: true,
			Logger:                 logger.Discard,
		})
	})
	return dry, dryErr
}

// Build runs build against the DryRun instance and returns the statement it
// produced: SQL with $n placeholders and the arguments in order. build must
// end in a finisher (Find, First, Create, Updates, Delete, Exec ...), since
// GORM renders SQL only there.
func Build(build func(*gorm.DB) *gorm.DB) (string, []any, error) {
	db, err := DB()
	if err != nil {
		return "", nil, err
	}
	tx := build(db.Session(&gorm.Session{NewDB: true}))
	if tx == nil {
		return "", nil, errors.New("gormtest: build returned nil")
	}
	query := tx.Statement.SQL.String()
	// Scan and Rows render the statement and then report that DryRun cannot
	// return rows; the statement is what is wanted here.
	if err := tx.Error; err != nil && !(errors.Is(err, gorm.ErrDryRunModeUnsupported) && query != "") {
		return "", nil, err
	}
	if query == "" {
		return "", nil, errors.New("gormtest: no SQL was built; end build with a finisher such as Find or Create")
	}
	return query, tx.Statement.Vars, nil
}

// Assert compares the statement build produces with
// testdata/<t.Name()>.golden, relative to the test's package directory, and
// rewrites the file instead when the test runs with -update.
func Assert(t testing.TB, build func(*gorm.DB) *gorm.DB) {
	t.Helper()
	query, vars, err := Build(build)
	if err != nil {
		t.Fatal(err)
	}
	got := Render(query, vars)
	path := filepath.Join("testdata", filepath.FromSlash(t.Name())+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no snapshot %s; create it with: go test -run '^%s$' -update", path, t.Name())
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("SQL differs from %s (rerun with -update if the change is intended)\n--- want\n%s--- got\n%s", path, want, got)
	}
}

// Render is the snapshot text: the SQL, then one line per argument with its
// placeholder, Go type and value. The type is part of the snapshot because
// it decides the encoding: store.Int64s is one array parameter where a
// plain []int64 would have been expanded into a list.
func Render(query string, vars []any) string {
	var b strings.Builder
	b.WriteString("-- sql\n")
	b.WriteString(strings.TrimSpace(query))
	b.WriteString("\n-- vars\n")
	for i, v := range vars {
		fmt.Fprintf(&b, "$%d %s\n", i+1, formatVar(v))
	}
	return b.String()
}

// formatVar prints an argument deterministically: pointers are followed
// (their address would change every run) and strings are quoted so that
// empty strings and spaces stay visible.
func formatVar(v any) string {
	if v == nil {
		return "nil"
	}
	typ := fmt.Sprintf("%T", v)
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return typ + " nil"
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return fmt.Sprintf("%s %q", typ, rv.String())
	}
	return fmt.Sprintf("%s %v", typ, rv.Interface())
}
