package moderation

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
)

// The statements of this package are built by GORM on the models of
// internal/model: reads select into a model or a small struct of the
// columns they read, writes go through Create, Updates and Delete, upserts
// through clause.OnConflict. SQL text is a constant, values are ?
// parameters, arrays pgdb.Array (GORM would expand a plain slice into a
// list of parameters). The *gorm.DB a function gets carries the context
// its statements run under (Host.DB binds it).

// configState selects the one config_state row.
func configState(db *gorm.DB) *gorm.DB {
	return db.Model(&model.ConfigState{}).Where("id = ?", model.ConfigStateID)
}

// forUpdate locks the rows a query reads until the transaction ends.
func forUpdate() clause.Locking { return clause.Locking{Strength: clause.LockingStrengthUpdate} }

// columns names the conflict target of an upsert or a RETURNING list.
func columns(names ...string) []clause.Column {
	out := make([]clause.Column, len(names))
	for i, n := range names {
		out[i] = clause.Column{Name: n}
	}
	return out
}

// assign sets column to the SQL expression expr in an upsert.
func assign(column string, expr pgdb.SQL) clause.Assignment {
	return clause.Assignment{Column: clause.Column{Name: column}, Value: gorm.Expr(string(expr))}
}
