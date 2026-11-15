package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// Conds ANDs its conditions in the order added, a scope included; none
// leaves the statement without a WHERE.
func TestSnapshotConds(t *testing.T) {
	var f store.Conds
	f.And("name ILIKE ?", "%a%")
	f = append(f, func(db *gorm.DB) *gorm.DB { return db.Where("owner = ?", 7) })
	f.And("refs && ?", store.Int64s{1, 2})
	t.Run("some", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Table("widgets").Scopes(f.Scope()).Find(&[]widget{}) })
	})
	t.Run("none", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Table("widgets").Scopes(store.Conds(nil).Scope()).Find(&[]widget{})
		})
	})
}

func TestSetIf(t *testing.T) {
	name, zero := "a", 0
	set := map[string]any{}
	store.SetIf(set, "name", &name)
	store.SetIf(set, "port", &zero)
	store.SetIf[string](set, "host", nil)
	assert.Equal(t, map[string]any{"name": "a", "port": 0}, set)
}

func TestNullStrings(t *testing.T) {
	assert.Nil(t, store.NullStrings(nil))
	assert.Equal(t, store.Strings{}, store.NullStrings([]string{}))
	assert.Equal(t, store.Strings{"a"}, store.NullStrings([]string{"a"}))
}
