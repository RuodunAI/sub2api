// Package model is the schema of plg_anthropic as GORM models: the one
// description of its tables. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
package model

import "time"

// All lists every model, for sub2api-plugin db.
func All() []any { return []any{&ModelCatalog{}} }

// ModelCatalog is one model of the catalog GET /models serves.
type ModelCatalog struct {
	ID int64 `gorm:"primaryKey"`
	// unique is the column's single-column UNIQUE constraint
	// (model_catalog_model_id_key, created by 0001).
	ModelID         string    `gorm:"type:varchar(100);not null;unique"`
	DisplayName     string    `gorm:"type:varchar(200);not null"`
	ContextWindow   int       `gorm:"type:integer;not null"`
	MaxOutputTokens int       `gorm:"type:integer;not null"`
	Status          string    `gorm:"type:varchar(20);not null;default:'active'"` // active | legacy
	SortOrder       int       `gorm:"type:integer;not null;default:0"`
	CreatedAt       time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt       time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (ModelCatalog) TableName() string { return "model_catalog" }
