package pgdbtool

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// dialector is what the dialector of a pgdb handle implements.
type dialector interface {
	gorm.Dialector
	gorm.ErrorTranslator
	gorm.SavePointerDialectorInterface
}

// driftDialector is the dialector of a pgdb handle with driftMigrator.
type driftDialector struct{ dialector }

// wrapDialector installs driftDialector on db, after pgdb.Open (gorm.Open
// applied the postgres dialector's own options there, e.g. the identifier
// length that truncates long index names as the host's does).
func wrapDialector(db *gorm.DB) error {
	d, ok := db.Dialector.(dialector)
	if !ok {
		return fmt.Errorf("pgdbtool: dialector %T has no savepoints or error translation", db.Dialector)
	}
	db.Dialector = driftDialector{d}
	return nil
}

func (d driftDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return driftMigrator{d.dialector.Migrator(db).(postgres.Migrator)}
}

// driftMigrator compares column defaults by meaning (probedColumn) and
// leaves a unique constraint of another name than GORM's to the catalog
// comparison.
type driftMigrator struct{ postgres.Migrator }

func (m driftMigrator) MigrateColumn(value any, field *schema.Field, column gorm.ColumnType) error {
	return m.Migrator.MigrateColumn(value, field, probedColumn{column, field, m})
}

// MigrateColumnUnique: when a model drops `unique` from a field, GORM drops
// the constraint uni_<table>_<column>, the only name it knows; the
// migrations may have named it otherwise (UNIQUE in CREATE TABLE makes
// <table>_<column>_key), and the drop fails. Such a constraint is left in
// place, and Drift reports it by its real name and definition.
func (m driftMigrator) MigrateColumnUnique(value any, field *schema.Field, column gorm.ColumnType) error {
	if unique, ok := column.Unique(); ok && unique && !field.Unique &&
		!m.HasConstraint(value, m.DB.NamingStrategy.UniqueName(field.Schema.Table, field.DBName)) {
		return nil
	}
	return m.Migrator.MigrateColumnUnique(value, field, column)
}

// probedColumn is a column whose DefaultValue is the model's spelling when
// PostgreSQL reads both the same. GORM compares the stored default, cut at
// its first "::" and unquoted ('{}'::jsonb reads {}, ARRAY['a'::text] reads
// ARRAY['a'), with the tag as written, so a default of most types other than
// text would always look changed and AutoMigrate would write it again.
type probedColumn struct {
	storedColumn
	field *schema.Field
	m     driftMigrator
}

// storedColumn is the column as the database has it. (Embedded as
// gorm.ColumnType, its field would be named ColumnType and hide the method
// of that name.)
type storedColumn interface{ gorm.ColumnType }

func (c probedColumn) DefaultValue() (string, bool) {
	v, ok := c.storedColumn.DefaultValue()
	if ok && v != c.field.DefaultValue && c.m.hasModelDefault(c.field) {
		return c.field.DefaultValue, true
	}
	return v, ok
}

// hasModelDefault reports whether the column of field has the model's
// default: the model's default is set on a temporary column of the
// column's type and its stored form (pg_get_expr) compared with the
// column's. Any error answers false: AutoMigrate then sets the model's
// default, and fails or shows the change.
func (m driftMigrator) hasModelDefault(field *schema.Field) bool {
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
	// Its statements are no changes: they are not recorded.
	probe := m.DB.WithContext(context.WithValue(m.DB.Statement.Context, recorder{}, (*[]string)(nil)))
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
