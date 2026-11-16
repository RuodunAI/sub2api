// Package gormtest snapshots the SQL that GORM builds, without a database.
//
// Every statement of the core is built by GORM (see the GORM notes in
// package store): what a repository sends to PostgreSQL is fully decided by
// the query builder, and a golden file of its SQL and arguments is the review surface for a migration: the
// diff shows exactly how a statement changed. Assert compares a query with
// testdata/<test name>.golden; `go test -run <test> -update` rewrites it.
package gormtest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// update is registered by this package, so a test package that imports it
// gets -update without declaring it (and must not declare its own).
var update = flag.Bool("update", false, "rewrite the SQL snapshots under testdata")

// Updating reports whether the test runs with -update: a package with
// golden files of its own next to the snapshots rewrites them on the same
// flag, which it cannot declare again.
func Updating() bool { return *update }

// DB returns a DryRun session of an instance configured as store.Open's
// (store.DryRun). The snapshots therefore describe the statements built in
// production (same configuration, callbacks and clause builders), and a
// statement that escaped DryRun would fail with store.ErrOffline, never
// reach a server.
func DB() *gorm.DB {
	return store.DryRun().WithContext(context.Background())
}

// Build runs build against a DryRun session and returns the statement it
// produced: SQL with $n placeholders and the arguments in order. build must
// end in a finisher (Find, First, Create, Updates, Delete, Exec ...), since
// GORM renders SQL only there.
func Build(build func(*gorm.DB) *gorm.DB) (string, []any, error) {
	tx := build(DB().Session(&gorm.Session{NewDB: true}))
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
	compare(t, Render(query, vars))
}

// AssertRun is Assert for a function that runs its statements rather than
// returning one, or runs several: every statement run sends to its DryRun
// session, in order. A read ends in gorm.ErrDryRunModeUnsupported (DryRun
// returns no rows), which is not a failure here. The session stands for a
// transaction's (store.RequireTx passes), so that the functions that lock
// can be snapshot; store.DryRun and DB are no transaction.
func AssertRun(t testing.TB, run func(*gorm.DB) error) {
	t.Helper()
	rec := &recorder{}
	s := DB().Session(&gorm.Session{NewDB: true, Context: context.WithValue(context.Background(), recorderKey{}, rec)})
	s.Statement.ConnPool = dryTx{s.Statement.ConnPool}
	err := run(s)
	if err != nil && !errors.Is(err, gorm.ErrDryRunModeUnsupported) {
		t.Fatal(err)
	}
	if len(rec.stmts) == 0 {
		t.Fatal("gormtest: run sent no statement")
	}
	compare(t, strings.Join(rec.stmts, "\n"))
}

// dryTx is the connection pool of AssertRun's session as a transaction's
// (gorm.TxCommitter). DryRun sends nothing to it.
type dryTx struct{ gorm.ConnPool }

func (dryTx) Commit() error   { return nil }
func (dryTx) Rollback() error { return nil }

// recorder collects the statements of the session whose context holds it.
type recorder struct{ stmts []string }

type recorderKey struct{}

// init adds a callback after each finisher of the DryRun instance that
// appends the statement to the recorder of its context; a session without
// one (Assert's) is untouched. At init, since registering recompiles the
// callback chains other (parallel) tests run.
func init() {
	record := func(db *gorm.DB) {
		if rec, ok := db.Statement.Context.Value(recorderKey{}).(*recorder); ok && db.Statement.SQL.Len() > 0 {
			rec.stmts = append(rec.stmts, Render(db.Statement.SQL.String(), db.Statement.Vars))
		}
	}
	cb := DB().Callback()
	for name, p := range map[string]interface {
		Register(string, func(*gorm.DB)) error
	}{
		"create": cb.Create().After("gorm:create"), "query": cb.Query().After("gorm:query"),
		"update": cb.Update().After("gorm:update"), "delete": cb.Delete().After("gorm:delete"),
		"row": cb.Row().After("gorm:row"), "raw": cb.Raw().After("gorm:raw"),
	} {
		if err := p.Register("gormtest:record_"+name, record); err != nil {
			panic("gormtest: register the recorder: " + err.Error())
		}
	}
}

// compare checks got against testdata/<t.Name()>.golden, or rewrites the
// file under -update.
func compare(t testing.TB, got string) {
	t.Helper()
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
