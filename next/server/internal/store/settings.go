package store

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpdateSettingJSONTx serializes partial configuration updates, including the
// first insert. The callback sees the latest committed document under the row
// lock and must return a complete validated JSON object. Callers broadcast only
// after their transaction commits.
func UpdateSettingJSONTx(ctx context.Context, tx *gorm.DB, key string, updatedBy *int64, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	if err := RequireTx(tx); err != nil {
		return nil, err
	}
	db := G(ctx, tx)
	if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).
		Create(&newSetting{Key: key, Value: json.RawMessage("{}")}).Error; err != nil {
		return nil, err
	}
	raw, err := SettingValue(db, key, ForUpdate())
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

// newSetting is a settings row with its first document; updated_at and
// updated_by are the defaults, and nothing is read back.
type newSetting struct {
	Key   string          `gorm:"column:key"`
	Value json.RawMessage `gorm:"column:value"`
}

func (newSetting) TableName() string { return "settings" }

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
	return db.Tx(ctx, func(tx *gorm.DB) error { return PatchSettingJSONTx(ctx, tx, key, updatedBy, patch, into) })
}

// PatchSettingJSONTx is PatchSettingJSON in the caller's transaction, for a
// caller that writes more in it (the settings.changed event).
func PatchSettingJSONTx(ctx context.Context, tx *gorm.DB, key string, updatedBy *int64, patch, into any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, err = UpdateSettingJSONTx(ctx, tx, key, updatedBy, func(raw json.RawMessage) (json.RawMessage, error) {
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
}
