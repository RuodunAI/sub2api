package providerresources

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the provider resource statements built with GORM (see package
// gormtest).

var (
	snapOwner   = core.ResourceOwner{UserID: 7, GroupID: 3}
	snapBinding = core.ResourceBinding{AccountID: 11, PrincipalID: "p", Generation: "g1"}
	snapTime    = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
)

func TestSQLResourcesReadLocked(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row model.ProviderResource
		return resources(db.Clauses(store.ForUpdate())).Where(`user_id=? AND group_id=? AND public_id=?`, 7, 3, "s2res_1").Take(&row)
	})
}

func TestSQLResourcesOwnedRemote(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row model.ProviderResource
		return resources(db).Where(ownedRemote, 7, 3, "skill", 11, "p", "g1", "skill_01").Take(&row)
	})
}

func TestSQLResourcesQuota(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var n int64
		return occupied(db, snapOwner, snapTime).Scan(&n)
	})
}

func TestSQLResourcesContextQuota(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var n int64
		return liveContexts(db, snapOwner, snapTime, "s2res_1").Count(&n)
	})
}

func TestSQLResourcesAdvisoryLock(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return advisoryLock(db, "provider-resources:{}") })
}

// A page after a cursor, with every filter of queryFilter.
func TestSQLResourcesQueryPage(t *testing.T) {
	where, args, err := queryFilter(snapOwner, core.ResourceQuery{PluginKey: "ccgateway", Kind: "file", AccountIDs: []int64{11, 12},
		IDs: []string{"s2res_1"}, Limit: 20, ReadyOnly: true, ExpiredWithin: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	where += ` AND (created_at,public_id)<(?,?)`
	args = append(args, snapTime, "s2res_9")
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []model.ProviderResource
		return page(db, where, args, "DESC", 21).Find(&rows)
	})
}

func TestSQLResourcesReserve(t *testing.T) {
	in := core.ResourceIntent{RequestID: "req", PluginKey: "ccgateway", Kind: "file", Owner: snapOwner, Binding: snapBinding,
		Bytes: 42, Metadata: json.RawMessage(`{}`)}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		row := newRow("s2res_1", "op", in, "pending")
		row.IntentHash, row.ExpiresAt = "hash", &snapTime
		return db.Create(&row)
	})
}

func TestSQLResourcesRegisterObserved(t *testing.T) {
	in := core.ResourceObservation{Owner: snapOwner, Binding: snapBinding, PluginKey: "ccgateway", Kind: "container", RemoteID: "cntr_1"}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(observedRow("s2res_1", "op", "key", in, json.RawMessage(`{}`), nil))
	})
}

func TestSQLResourcesBindContext(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(&model.ProviderResourceContext{UserID: 7, GroupID: 3, AccountID: 11, PrincipalID: "p", Generation: "g1",
			PluginKey: "ccgateway", Kind: "ptc", ParentID: "msg_1", ResourceID: "s2res_1"})
	})
}

// Every state change writes the columns of the raw SQL it replaced and
// moves updated_at to now(), nothing else.
func TestSQLResourcesUpdate(t *testing.T) {
	expires := snapTime
	for name, c := range map[string]struct {
		table string
		set   map[string]any
	}{
		"finalize":         {resourcesTable, map[string]any{"state": "ready", "remote_id": "file_1", "bytes": int64(42), "metadata": json.RawMessage(`{}`), "expires_at": &expires}},
		"uncertain":        {resourcesTable, map[string]any{"state": "uncertain"}},
		"fail_create":      {resourcesTable, map[string]any{"state": "failed", "failure_code": "permission_error"}},
		"begin_delete":     {resourcesTable, map[string]any{"state": "deleting", "operation_id": "op"}},
		"observe":          {resourcesTable, map[string]any{"metadata": json.RawMessage(`{}`), "bytes": int64(42), "expires_at": (*time.Time)(nil)}},
		"skill_parent":     {resourcesTable, map[string]any{"remote_id": "skill_1", "state": "ready", "metadata": json.RawMessage(`{}`)}},
		"skill_version":    {versionsTable, map[string]any{"remote_id": "v1", "legacy_epoch": "", "state": "ready", "metadata": json.RawMessage(`{}`), "created_at": snapTime}},
		"skill_mark":       {versionsTable, map[string]any{"state": "uncertain", "metadata": json.RawMessage(`{}`)}},
		"skill_delete":     {versionsTable, map[string]any{"state": "deleting", "operation_id": "op"}},
		"skill_delete_end": {versionsTable, map[string]any{"state": "deleted"}},
	} {
		t.Run(name, func(t *testing.T) {
			given := maps.Clone(c.set)
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return update(db, c.table, "s2res_1", c.set) })
			if !reflect.DeepEqual(c.set, given) {
				t.Fatalf("update changed the caller's map: %v, was %v", c.set, given)
			}
		})
	}
}

func TestSQLResourcesDeleteVersions(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return deleteVersions(db, "s2res_1") })
}

func TestSQLResourcesSkillVersions(t *testing.T) {
	t.Run("latest", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var row model.ProviderSkillVersion
			return skillVersions(db.Order(newestFirst)).Where(`parent_id=? AND state='ready'`, "s2res_1").Take(&row)
		})
	})
	t.Run("page", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var rows []model.ProviderSkillVersion
			return readyVersions(db, "s2res_1", &core.SkillVersion{PublicVersionID: "s2skv_9", CreatedAt: snapTime}, 21).Find(&rows)
		})
	})
	t.Run("create", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Create(&model.ProviderSkillVersion{PublicID: "s2skv_1", ParentID: "s2res_1", RequestID: "req", State: "pending",
				OperationID: "op", Bytes: 42, IntentHash: "hash"})
		})
	})
	t.Run("busy", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var busy bool
			return busyVersions(db, "s2res_1").Scan(&busy)
		})
	})
}
