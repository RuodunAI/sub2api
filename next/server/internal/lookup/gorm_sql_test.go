package lookup

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL snapshots of the lookup statements (testdata/*.golden).

func kind(t *testing.T, name string) *Entity {
	t.Helper()
	e, ok := New(nil, nil).kinds[name]
	if !ok {
		t.Fatalf("no kind %s", name)
	}
	return e
}

// A q of digits on users: the name, Search columns and the id match, and
// the row of that id comes first.
func TestSQLQueryUsersByText(t *testing.T) {
	e := kind(t, "users")
	id := int64(42)
	var f store.Conds
	f.And(e.Live)
	p := &params{text: "42", id: &id, limit: 20, refs: map[string]int64{}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return e.query(db, p, f, e.Hint, &[]itemRow{}) })
}

// Accounts within a data scope, preferring one group's.
func TestSQLQueryAccountsScoped(t *testing.T) {
	e := kind(t, "accounts")
	var f store.Conds
	f.And(e.Live)
	f = append(f, e.Table.Scope(core.DataScope{Owner: 7, Groups: []int64{0, 3}, IDs: []int64{5}}))
	p := &params{limit: 10, refs: map[string]int64{"prefer_group_id": 3}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return e.query(db, p, f, e.Hint, &[]itemRow{}) })
}

// Models by name: exactly these rows, no limit.
func TestSQLQueryModelsByIDs(t *testing.T) {
	e := kind(t, "models")
	var f store.Conds
	f.And("mp.model = ANY(?)", store.Strings{"gpt-x"})
	p := &params{ids: store.Strings{"gpt-x", "gpt-y"}, limit: 20, refs: map[string]int64{}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return e.query(db, p, f, e.Hint, &[]itemRow{}) })
}

func TestSQLPolicyRows(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return policyRows(db, 7, &[]policyRow{}) })
}
