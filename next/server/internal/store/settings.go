package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpdateSettingJSONTx serializes partial configuration updates, including the
// first insert. The callback sees the latest committed document under the row
// lock and must return a complete validated JSON object. Callers broadcast only
// after their transaction commits.
func UpdateSettingJSONTx(ctx context.Context, tx pgx.Tx, key string, updatedBy *int64, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	db := G(ctx, tx)
	if err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,'{}') ON CONFLICT(key) DO NOTHING`, key).Error; err != nil {
		return nil, err
	}
	raw, err := SettingValue(db, key, ForUpdate)
	if err != nil {
		return nil, err
	}
	next, err := update(raw)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(next, &obj); err != nil || obj == nil {
		return nil, errors.New("setting must be a JSON object")
	}
	return next, writeSetting(db, key, next, updatedBy).Error
}

// SettingValue reads the document of settings row key; a missing row is an
// IsNoRows error. clauses go into the SELECT: ForUpdate locks the row until
// the transaction ends.
func SettingValue(db *gorm.DB, key string, clauses ...clause.Expression) (json.RawMessage, error) {
	var raw []byte
	err := settingValue(db, key).Clauses(clauses...).Row().Scan(&raw)
	return raw, err
}

// settingValue selects the document of settings row key.
func settingValue(db *gorm.DB, key string) *gorm.DB {
	return db.Table("settings").Select("value").Where("key = ?", key)
}

// writeSetting stores next as the document of settings row key.
func writeSetting(db *gorm.DB, key string, next json.RawMessage, updatedBy *int64) *gorm.DB {
	return db.Table("settings").Where("key = ?", key).
		Updates(map[string]any{"value": next, "updated_by": updatedBy, "updated_at": gorm.Expr("now()")})
}

// PatchSettingJSON merges non-null supplied fields under the same transaction
// lock. into must contain defaults; it receives the committed effective value.
// Validate patch values before calling (and use UpdateSettingJSONTx directly
// when validation depends on several merged fields).
func PatchSettingJSON(ctx context.Context, db *DB, key string, updatedBy *int64, patch, into any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	return db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := UpdateSettingJSONTx(ctx, tx, key, updatedBy, func(raw json.RawMessage) (json.RawMessage, error) {
			var current map[string]json.RawMessage
			if err := json.Unmarshal(raw, &current); err != nil {
				return nil, err
			}
			if current == nil {
				current = map[string]json.RawMessage{}
			}
			for key, value := range fields {
				if string(value) != "null" {
					current[key] = value
				}
			}
			merged, err := json.Marshal(current)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(merged, into); err != nil {
				return nil, err
			}
			return json.Marshal(into)
		})
		return err
	})
}
