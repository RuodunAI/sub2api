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

// maxAutoMigrate bounds the AutoMigrate runs of Drift.
const maxAutoMigrate = 4

// DriftReport is how a plugin's GORM models differ from its migrations.
type DriftReport struct {
	// Statements is the DDL AutoMigrate ran to bring the migrated schema to
	// the models, in order.
	Statements []string `json:"statements"`
	// Unmapped is what the migrations created and no model maps: "table t",
	// "column t.c", "index t.i", "constraint t.c <definition>" (a check,
	// foreign key or unique constraint), "view v", "sequence s",
	// "function f(args)". AutoMigrate never drops anything, so a field or
	// model removed from the code shows up only here.
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
	// The comparisons of sameDefault are not changes either.
	if err := db.Callback().Raw().After("gorm:raw").Register("pgdb:drift", func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Context.Value(probing{}) == nil {
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
		// One AutoMigrate need not reach the models: a column it adds gets
		// its unique constraint on the next run only. Run it until it
		// changes nothing, so that the DDL is all a migration needs.
		for pass := 0; ; pass++ {
			ran := len(ddl)
			if err := tx.AutoMigrate(models...); err != nil {
				return fmt.Errorf("pgdb: AutoMigrate: %w", err)
			}
			if len(ddl) == ran {
				break
			}
			if pass == maxAutoMigrate {
				return fmt.Errorf("pgdb: AutoMigrate still changes the schema after %d runs: %q", pass+1, ddl[ran:])
			}
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

// unmapped lists what the current schema has and no model maps.
func unmapped(tx *gorm.DB, models []any) ([]string, error) {
	tables, err := tx.Migrator().GetTables()
	if err != nil {
		return nil, err
	}
	out := []string{}
	schemas := map[string]*schema.Schema{}
	for _, model := range models {
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(model); err != nil {
			return nil, err
		}
		schemas[stmt.Schema.Table] = stmt.Schema
		columns, err := tx.Migrator().ColumnTypes(model)
		if err != nil {
			return nil, err
		}
		for _, c := range columns {
			if _, ok := stmt.Schema.FieldsByDBName[c.Name()]; !ok {
				out = append(out, "column "+stmt.Schema.Table+"."+c.Name())
			}
		}
	}
	for _, t := range tables {
		if schemas[t] == nil {
			out = append(out, "table "+t)
		}
	}
	// Constraints and indexes of the mapped tables, by name (mapsObject):
	// that is how AutoMigrate knows them. Primary keys come with the table,
	// NOT NULL (contype n, PostgreSQL 18) with the column; an index that
	// backs a constraint is the constraint.
	var objects []struct{ Kind, Table, Name, Type, Def, Column string }
	err = tx.Raw(`SELECT 'constraint' AS kind, t.relname AS table, c.conname AS name, c.contype::text AS type, pg_get_constraintdef(c.oid) AS def,
			COALESCE((SELECT a.attname FROM pg_attribute a WHERE a.attrelid = c.conrelid AND a.attnum = c.conkey[1] AND cardinality(c.conkey) = 1), '') AS column
		FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relnamespace = current_schema()::regnamespace AND c.contype NOT IN ('p', 'n')
		UNION ALL
		SELECT 'index', t.relname, i.relname, '', '', ''
		FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
		WHERE t.relnamespace = current_schema()::regnamespace
			AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = x.indexrelid AND c.conrelid = x.indrelid)`).Scan(&objects).Error
	if err != nil {
		return nil, err
	}
	for _, o := range objects {
		s := schemas[o.Table]
		switch {
		case s == nil || mapsObject(s, o.Type, o.Name, o.Column):
		case o.Kind == "index":
			out = append(out, "index "+o.Table+"."+o.Name)
		default:
			out = append(out, "constraint "+o.Table+"."+o.Name+" "+o.Def)
		}
	}
	// What no model can describe at all. A sequence of a serial or identity
	// column belongs to its column; objects of an extension to the extension.
	var others []string
	err = tx.Raw(`SELECT CASE c.relkind WHEN 'S' THEN 'sequence ' WHEN 'm' THEN 'materialized view ' ELSE 'view ' END || c.relname
		FROM pg_class c
		WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind IN ('S', 'v', 'm')
			AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('a', 'i', 'e'))
		UNION ALL
		SELECT 'function ' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
		FROM pg_proc p
		WHERE p.pronamespace = current_schema()::regnamespace
			AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')`).Scan(&others).Error
	if err != nil {
		return nil, err
	}
	out = append(out, others...)
	slices.Sort(out)
	return out, nil
}

// mapsObject reports whether model s declares the constraint (contype typ)
// or, for typ "", the index called name. column is the constraint's column
// when it has only one.
func mapsObject(s *schema.Schema, typ, name, column string) bool {
	switch typ {
	case "":
		return s.LookIndex(name) != nil
	case "c":
		_, ok := s.ParseCheckConstraints()[name]
		return ok
	case "f":
		for _, rel := range s.Relationships.Relations {
			if c := rel.ParseConstraint(); c != nil && c.Schema == s && c.Name == name {
				return true
			}
		}
	case "u":
		// Like AutoMigrate, take any unique constraint on just the column of a
		// `unique` field for the field's (GORM itself would name it
		// uni_<table>_<column>). A uniqueIndex of the constraint's name is
		// satisfied by the constraint's index.
		if f := s.FieldsByDBName[column]; f != nil && f.Unique {
			return true
		}
		return s.LookIndex(name) != nil
	}
	// Exclusion constraints and constraint triggers have no GORM tag.
	return false
}

// driftDialector is the postgres dialect with driftMigrator.
type driftDialector struct{ gorm.Dialector }

func (d driftDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return driftMigrator{d.Dialector.Migrator(db).(postgres.Migrator)}
}

// driftMigrator compares column defaults by meaning (sameDefault) and leaves
// a unique constraint of another name than GORM's to unmapped.
type driftMigrator struct{ postgres.Migrator }

func (m driftMigrator) MigrateColumn(value any, field *schema.Field, column gorm.ColumnType) error {
	return m.Migrator.MigrateColumn(value, field, sameDefault{column, field, m})
}

// MigrateColumnUnique: when a model drops `unique` from a field, GORM drops
// the constraint uni_<table>_<column>, the only name it knows; the
// migrations may have named it otherwise (UNIQUE in CREATE TABLE makes
// <table>_<column>_key), and the drop fails. Such a constraint is left in
// place, and unmapped reports it by its real name and definition.
func (m driftMigrator) MigrateColumnUnique(value any, field *schema.Field, column gorm.ColumnType) error {
	if unique, ok := column.Unique(); ok && unique && !field.Unique &&
		!m.HasConstraint(value, m.DB.NamingStrategy.UniqueName(field.Schema.Table, field.DBName)) {
		return nil
	}
	return m.Migrator.MigrateColumnUnique(value, field, column)
}

// probing marks the context of sameDefault's statements, which Drift does
// not record.
type probing struct{}

// sameDefault answers the column's default in the model's spelling when
// PostgreSQL reads both the same. GORM compares the stored default, cut at
// its first "::" and unquoted ('{}'::jsonb reads {}, ARRAY['a'::text] reads
// ARRAY['a'), with the tag as written, so a default of most types other than
// text would always look changed. Text cannot say whether two defaults
// mean the same; PostgreSQL can: the model's default is set on a temporary
// column of the column's type and its stored form (pg_get_expr) compared
// with the column's.
type sameDefault struct {
	columnType
	field *schema.Field
	m     driftMigrator
}

// columnType names the embedded gorm.ColumnType apart from its method
// ColumnType, which the field would otherwise hide.
type columnType = gorm.ColumnType

func (c sameDefault) DefaultValue() (string, bool) {
	v, ok := c.columnType.DefaultValue()
	if ok && v != c.field.DefaultValue && c.m.sameDefault(c.field) {
		return c.field.DefaultValue, true
	}
	return v, ok
}

// sameDefault reports whether the column of field has the model's default.
// Any error answers false: AutoMigrate then sets the model's default, and
// fails or shows the change.
func (m driftMigrator) sameDefault(field *schema.Field) bool {
	expr := modelDefault(m.DB, field)
	if expr == "" {
		return false
	}
	var typ, stored string
	err := m.DB.Raw(`SELECT format_type(a.atttypid, a.atttypmod), pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = to_regclass(quote_ident(?)) AND a.attname = ?`, field.Schema.Table, field.DBName).Row().Scan(&typ, &stored)
	if err != nil {
		return false
	}
	// A savepoint inside Drift's transaction: rolling back to it drops the
	// temporary table and survives a default that does not fit the type.
	probe := m.DB.WithContext(context.WithValue(m.DB.Statement.Context, probing{}, true))
	if probe.Exec("SAVEPOINT pgdb_default_probe").Error != nil {
		return false
	}
	defer probe.Exec("ROLLBACK TO SAVEPOINT pgdb_default_probe; RELEASE SAVEPOINT pgdb_default_probe")
	var probed string
	err = probe.Exec("CREATE TEMPORARY TABLE pgdb_default_probe (v " + typ + " DEFAULT " + expr + ")").Error
	if err == nil {
		err = probe.Raw(`SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'pg_temp.pgdb_default_probe'::regclass`).Row().Scan(&probed)
	}
	return err == nil && probed == stored
}

// modelDefault is the DEFAULT clause GORM writes for field (migrator's
// FullDataTypeOf), "" for none or NULL.
func modelDefault(db *gorm.DB, field *schema.Field) string {
	switch {
	case !field.HasDefaultValue || field.DefaultValue == "(-)" || strings.EqualFold(field.DefaultValue, "null"):
		return ""
	case field.DefaultValueInterface != nil:
		stmt := &gorm.Statement{Vars: []any{field.DefaultValueInterface}}
		db.Dialector.BindVarTo(stmt, stmt, field.DefaultValueInterface)
		return db.Dialector.Explain(stmt.SQL.String(), field.DefaultValueInterface)
	}
	return field.DefaultValue
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
