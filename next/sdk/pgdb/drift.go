package pgdb

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// DriftDSNEnv names the variable that hands DriftMain the server DSN; it is
// not an argument so that the password stays out of the process list.
const DriftDSNEnv = "SUB2API_DRIFT_DSN"

// DriftReport is how a plugin's GORM models differ from its migrations.
type DriftReport struct {
	// Statements is the DDL AutoMigrate ran to bring the migrated schema to
	// the models, in order.
	Statements []string `json:"statements"`
	// Unmapped is what the migrations created and no model maps ("table t",
	// "column t.c", "index t.i"). AutoMigrate never drops anything, so a
	// field or model removed from the code shows up only here.
	Unmapped []string `json:"unmapped"`
}

// Clean reports whether the models and the migrations agree.
func (r *DriftReport) Clean() bool { return len(r.Statements) == 0 && len(r.Unmapped) == 0 }

// Drift applies migrations to a scratch schema on the server at dsn (a
// role that may create schemas), runs GORM's AutoMigrate for models on it
// and reports the DDL that ran and what no model maps. The scratch schema is
// dropped afterwards.
func Drift(ctx context.Context, dsn string, migrations []Migration, models []any) (*DriftReport, error) {
	// One connection is enough: everything below runs in turn.
	db, err := open(dsn, Options{MaxConns: 1}, func(d gorm.Dialector) gorm.Dialector { return driftDialector{d} })
	if err != nil {
		return nil, err
	}
	defer Close(db) //nolint:errcheck // nothing left to flush
	db = db.WithContext(ctx)

	scratch := fmt.Sprintf("sub2api_drift_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := CreateSchema(ctx, db, scratch); err != nil {
		return nil, fmt.Errorf("pgdb: create scratch schema: %w", err)
	}
	defer DropSchema(context.WithoutCancel(ctx), db, scratch) //nolint:errcheck // best effort cleanup
	if err := ApplyMigrations(ctx, db, scratch, migrations); err != nil {
		return nil, err
	}

	// Every Exec of the migrator is DDL (its reads go through Raw().Scan),
	// so recording the Execs that succeeded records exactly the changes.
	var ddl []string
	if err := db.Callback().Raw().After("gorm:raw").Register("pgdb:drift", func(tx *gorm.DB) {
		if tx.Error == nil {
			ddl = append(ddl, tx.Dialector.Explain(tx.Statement.SQL.String(), tx.Statement.Vars...))
		}
	}); err != nil {
		return nil, err
	}
	report := &DriftReport{}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL search_path TO ?", clause.Table{Name: scratch}).Error; err != nil {
			return err
		}
		ddl = nil // the SET above is not a change
		if err := tx.AutoMigrate(models...); err != nil {
			return fmt.Errorf("pgdb: AutoMigrate: %w", err)
		}
		report.Statements = append([]string{}, ddl...)
		report.Unmapped, err = unmapped(tx, models)
		return err
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// unmapped lists the tables, columns and indexes of the current schema that
// no model maps.
func unmapped(tx *gorm.DB, models []any) ([]string, error) {
	m := tx.Migrator()
	tables, err := m.GetTables()
	if err != nil {
		return nil, err
	}
	out := []string{}
	mapped := map[string]bool{}
	for _, model := range models {
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(model); err != nil {
			return nil, err
		}
		table := stmt.Schema.Table
		mapped[table] = true
		columns, err := m.ColumnTypes(model)
		if err != nil {
			return nil, err
		}
		for _, c := range columns {
			if _, ok := stmt.Schema.FieldsByDBName[c.Name()]; !ok {
				out = append(out, "column "+table+"."+c.Name())
			}
		}
		indexes, err := m.GetIndexes(model)
		if err != nil {
			return nil, err
		}
		for _, idx := range indexes {
			if stmt.Schema.LookIndex(idx.Name()) == nil {
				out = append(out, "index "+table+"."+idx.Name())
			}
		}
	}
	for _, t := range tables {
		if !mapped[t] {
			out = append(out, "table "+t)
		}
	}
	slices.Sort(out)
	return out, nil
}

// driftDialector is the postgres dialect with driftMigrator.
type driftDialector struct{ gorm.Dialector }

func (d driftDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return driftMigrator{d.Dialector.Migrator(db).(postgres.Migrator)}
}

// driftMigrator compares column defaults by meaning (sameDefault).
type driftMigrator struct{ postgres.Migrator }

func (m driftMigrator) MigrateColumn(value any, field *schema.Field, column gorm.ColumnType) error {
	return m.Migrator.MigrateColumn(value, field, sameDefault{column, field})
}

// sameDefault answers the column's default in the model's spelling when both
// mean the same. The driver strips the casts and quotes of the stored default
// ('{}'::jsonb reads {}) while GORM compares that with the tag as written
// ('{}'), unquoting it for text columns only: a quoted default of any other
// type (jsonb, an array, '-infinity') would always look changed.
type sameDefault struct {
	columnType
	field *schema.Field
}

// columnType names the embedded gorm.ColumnType apart from its method
// ColumnType, which the field would otherwise hide.
type columnType = gorm.ColumnType

func (c sameDefault) DefaultValue() (string, bool) {
	v, ok := c.columnType.DefaultValue()
	if ok && c.field.HasDefaultValue && v == unquoteDefault(c.field.DefaultValue) {
		return c.field.DefaultValue, true
	}
	return v, ok
}

func unquoteDefault(s string) string {
	if i := strings.Index(s, "::"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, "'")
}

// DriftMain is the main of the program sub2api-plugin db generates inside a
// plugin module (its internal/model package is out of reach elsewhere):
//
//	func main() { pgdb.DriftMain(model.All()) }
//
// It reads the server DSN from DriftDSNEnv and the migrations directory from
// its only argument, runs Drift and writes the DriftReport as JSON to stdout.
func DriftMain(models []any) {
	if err := driftMain(os.Args[1:], os.Stdout, models); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func driftMain(args []string, stdout io.Writer, models []any) error {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dsn := os.Getenv(DriftDSNEnv)
	if dsn == "" || fs.NArg() != 1 {
		return errors.New("usage: " + DriftDSNEnv + "=<dsn> drift <migrations dir>")
	}
	migrations, err := ReadMigrations(fs.Arg(0))
	if err != nil {
		return err
	}
	report, err := Drift(context.Background(), dsn, migrations, models)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(report)
}
