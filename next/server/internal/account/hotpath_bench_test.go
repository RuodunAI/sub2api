package account

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The account reads of the gateway's request path: the group snapshot of
// Candidates and the row of Load. Both are cached for snapshotTTL, so a
// request runs them on a miss only; they stay on pgx. Each is measured next
// to the GORM form of the same statement, against a real PostgreSQL
// (TEST_DATABASE_URL), as recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/account/

// hotFixture is a group of ten schedulable accounts; it returns the group
// and one of the accounts.
func hotFixture(b *testing.B) (*Service, int64, int64) {
	ctx := context.Background()
	db := testutil.DB(b)
	group := testutil.InsertGroup(b, db, "bench")
	user := testutil.InsertUser(b, db, "bench@test.invalid")
	var first int64
	for i := range 10 {
		var id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO accounts (name, plugin_key, type, credentials_enc, models, model_mapping, model_limits, environment, created_by)
			VALUES ('bench-' || $1::int, 'anthropic', 'apikey', '\x00', '{claude-sonnet-4-5,claude-opus-4-1}', '{"a":"b"}', '[]', '{"relay_timeout_seconds":60}', $2) RETURNING id`,
			i, user).Scan(&id); err != nil {
			b.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO account_groups (account_id, group_id) VALUES ($1, $2)`, id, group); err != nil {
			b.Fatal(err)
		}
		if first == 0 {
			first = id
		}
	}
	return New(Deps{DB: db}), group, first
}

// benchPaths runs the pgx and the GORM form of one read.
func benchPaths(b *testing.B, pgxPath, gormPath func() error) {
	for _, p := range []struct {
		name string
		run  func() error
	}{{"pgx", pgxPath}, {"gorm", gormPath}} {
		b.Run(p.name, func(b *testing.B) {
			if err := p.run(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := p.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHotGroupSnapshot(b *testing.B) {
	s, group, _ := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		_, err := pgxGroupMembers(ctx, s.d.DB, group)
		return err
	}, func() error {
		var members []schedulable
		if err := schedulableMembers(store.G(ctx, s.d.DB.Pool), group).Scan(&members).Error; err != nil {
			return err
		}
		for _, m := range members {
			_ = m.ref()
		}
		return nil
	})
}

func BenchmarkHotLoadRow(b *testing.B) {
	s, _, id := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		_, err := s.loadLive(ctx, id)
		return err
	}, func() error {
		_, err := scanRow(selectRows(store.G(ctx, s.d.DB.Pool)).Where("a.id = ? AND a.deleted_at IS NULL", id).Row())
		return err
	})
}

// pgxGroupMembers is the statement of groupSnapshot.
func pgxGroupMembers(ctx context.Context, db *store.DB, groupID int64) ([]core.AccountRef, error) {
	rows, err := db.Pool.Query(ctx, `SELECT a.id, a.name, a.plugin_key, a.type, a.priority, a.weight,
			a.max_concurrency, a.proxy_id, a.models, a.model_mapping, a.rpm_limit, a.tpm_limit, a.spm_limit, a.spm_window_seconds, a.model_limits, a.environment,
			COALESCE(a.conversion_loss_policy, ''), CASE WHEN a.status = 'auto_disabled' THEN a.auto_disabled_until END
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = $1 AND a.deleted_at IS NULL AND a.schedulable
			AND (a.status = 'active' OR a.status = 'auto_disabled' AND a.auto_disabled_until IS NOT NULL)
		ORDER BY a.priority DESC, a.id`, groupID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (core.AccountRef, error) {
		var a core.AccountRef
		var mapping, limits []byte
		var until *time.Time
		err := r.Scan(&a.ID, &a.Name, &a.PluginKey, &a.Type, &a.Priority, &a.Weight, &a.MaxConcurrency, &a.ProxyID,
			&a.Models, &mapping, &a.RPMLimit, &a.TPMLimit, &a.SPMLimit, &a.SPMWindowSeconds, &limits, &a.Environment, &a.ConversionLossPolicy, &until)
		if err == nil && len(mapping) > 0 {
			_ = json.Unmarshal(mapping, &a.ModelMapping)
		}
		a.ModelLimits = parseModelLimits(limits)
		return a, err
	})
}

// schedulableMembers is the GORM form of groupSnapshot's statement.
func schedulableMembers(db *gorm.DB, groupID int64) *gorm.DB {
	return db.Table("accounts a").Select(`a.id, a.name, a.plugin_key, a.type, a.priority, a.weight,
			a.max_concurrency, a.proxy_id, a.models, a.model_mapping, a.rpm_limit, a.tpm_limit, a.spm_limit, a.spm_window_seconds, a.model_limits, a.environment,
			COALESCE(a.conversion_loss_policy, '') AS conversion_loss_policy, CASE WHEN a.status = 'auto_disabled' THEN a.auto_disabled_until END AS release`).
		Joins("JOIN account_groups ag ON ag.account_id = a.id").
		Where(`ag.group_id = ? AND a.deleted_at IS NULL AND a.schedulable
			AND (a.status = 'active' OR a.status = 'auto_disabled' AND a.auto_disabled_until IS NOT NULL)`, groupID).
		Order("a.priority DESC, a.id")
}

// schedulable is a row of schedulableMembers.
type schedulable struct {
	ID                   int64
	Name                 string
	PluginKey            string
	Type                 string
	Priority             int
	Weight               int
	MaxConcurrency       int
	ProxyID              *int64
	Models               store.Strings
	ModelMapping         []byte
	RPMLimit             int   `gorm:"column:rpm_limit"`
	TPMLimit             int64 `gorm:"column:tpm_limit"`
	SPMLimit             int   `gorm:"column:spm_limit"`
	SPMWindowSeconds     int   `gorm:"column:spm_window_seconds"`
	ModelLimits          []byte
	Environment          store.JSON[core.EnvironmentValues]
	ConversionLossPolicy string
	Release              *time.Time
}

func (m schedulable) ref() core.AccountRef {
	a := core.AccountRef{ID: m.ID, Name: m.Name, PluginKey: m.PluginKey, Type: m.Type, Priority: m.Priority, Weight: m.Weight,
		MaxConcurrency: m.MaxConcurrency, ProxyID: m.ProxyID, Models: m.Models, RPMLimit: m.RPMLimit, TPMLimit: m.TPMLimit,
		SPMLimit: m.SPMLimit, SPMWindowSeconds: m.SPMWindowSeconds, ModelLimits: parseModelLimits(m.ModelLimits),
		Environment: m.Environment.V, ConversionLossPolicy: m.ConversionLossPolicy}
	if len(m.ModelMapping) > 0 {
		_ = json.Unmarshal(m.ModelMapping, &a.ModelMapping)
	}
	return a
}
