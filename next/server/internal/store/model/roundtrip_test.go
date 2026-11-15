package model_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// These tests check the models against a migrated database through
// store.G, the path production code takes.
func openGorm(t *testing.T) *gorm.DB {
	t.Helper()
	return store.G(context.Background(), testutil.DB(t).Pool)
}

func ptr[T any](v T) *T { return &v }

// A serial id and every defaulted column are left to the database and read
// back on insert; nullable columns round-trip nil and values.
func TestUserRoundTrip(t *testing.T) {
	g := openGorm(t)
	u := model.User{Email: "a@example.com", PasswordHash: "h"}
	require.NoError(t, g.Create(&u).Error)
	require.NotZero(t, u.ID)
	require.Equal(t, ptr("active"), u.Status)
	require.Equal(t, ptr(5), u.MaxConcurrency)
	require.NotZero(t, u.SegmentID) // default_segment_id()
	require.WithinDuration(t, time.Now(), u.CreatedAt, time.Minute)
	require.Equal(t, u.CreatedAt, u.UpdatedAt)
	require.Nil(t, u.LastLoginAt)

	var got model.User
	require.NoError(t, g.First(&got, u.ID).Error)
	require.Equal(t, u, got)

	login := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, g.Model(&got).Update("last_login_at", login).Error)
	var again model.User
	require.NoError(t, g.First(&again, u.ID).Error)
	require.NotNil(t, again.LastLoginAt)
	require.True(t, login.Equal(*again.LastLoginAt))
	require.True(t, again.UpdatedAt.After(u.UpdatedAt), "the update stamps updated_at from the database clock")

	// A nil pointer takes the default; a pointer to the zero value writes
	// it, even where the default is not zero.
	z := model.User{Email: "z@example.com", PasswordHash: "h", MaxConcurrency: ptr(0), Status: ptr("")}
	require.NoError(t, g.Create(&z).Error)
	var zr model.User
	require.NoError(t, g.First(&zr, z.ID).Error)
	require.Equal(t, ptr(0), zr.MaxConcurrency)
	require.Equal(t, ptr(""), zr.Status)
}

// numeric keeps every digit, jsonb round-trips as raw JSON, and an update
// assigns updated_at from the database clock.
func TestGroupRoundTrip(t *testing.T) {
	g := openGorm(t)
	grp := model.Group{
		Name:                 "g1",
		RateMultiplier:       ptr(decimal.RequireFromString("1.2345")),
		ModelAllowlist:       json.RawMessage(`["claude-x", "gpt-y"]`),
		ConversionLossPolicy: ptr("strict"),
	}
	require.NoError(t, g.Create(&grp).Error)
	require.Equal(t, "1.2345", grp.RateMultiplier.String())
	require.JSONEq(t, `{}`, string(grp.Environment))
	require.Equal(t, ptr("blacklist"), grp.ModelFilterMode)

	var got model.Group
	require.NoError(t, g.First(&got, grp.ID).Error)
	require.Equal(t, grp, got)
	require.JSONEq(t, `["claude-x","gpt-y"]`, string(got.ModelAllowlist))

	plain := model.Group{Name: "g2"}
	require.NoError(t, g.Create(&plain).Error)
	require.True(t, plain.RateMultiplier.Equal(decimal.NewFromInt(1)))
	require.JSONEq(t, `[]`, string(plain.ModelAllowlist))
	require.Nil(t, plain.ConversionLossPolicy)

	query, _, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&model.Group{ID: grp.ID}).Update("description", "x")
	})
	require.NoError(t, err)
	require.Equal(t, `UPDATE "groups" SET "description"=$1,"updated_at"=now() WHERE "id" = $2`, query)
}

// Nullable numeric and jsonb columns hold NULL as nil and values exactly.
func TestUsageLogRoundTrip(t *testing.T) {
	g := openGorm(t)
	base := decimal.RequireFromString("123456789012.12345678")
	log := model.UsageLog{
		RequestID: "r1", UserID: 1, APIKeyID: 1, GroupID: 1,
		Protocol: "anthropic.messages", Endpoint: "/v1/messages", StatusCode: 200, Success: true,
		TotalCost:     decimal.RequireFromString("0.00000001"),
		BaseCost:      &base,
		BillingDetail: json.RawMessage(`{"cost": "0.00000001"}`),
	}
	require.NoError(t, g.Create(&log).Error)
	require.Equal(t, ptr(1), log.Attempts)
	require.Nil(t, log.AccountID)
	require.Nil(t, log.Conversion)

	var got model.UsageLog
	require.NoError(t, g.First(&got, log.ID).Error)
	require.Equal(t, log, got)
	require.Equal(t, "123456789012.12345678", got.BaseCost.String())
	require.Equal(t, "0.00000001", got.TotalCost.String())

	var isNull bool
	require.NoError(t, g.Raw(`SELECT conversion IS NULL AND base_cost IS NOT NULL FROM usage_logs WHERE id = ?`, log.ID).Scan(&isNull).Error)
	require.True(t, isNull)
}

// A primary key without a sequence is always written, even when zero,
// instead of being treated as auto-increment.
func TestUserBalanceKeyIsWritten(t *testing.T) {
	g := openGorm(t)
	query, _, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB { return tx.Create(&model.UserBalance{}) })
	require.NoError(t, err)
	require.Contains(t, query, `"user_id"`)

	u := model.User{Email: "b@example.com", PasswordHash: "h"}
	require.NoError(t, g.Create(&u).Error)
	bal := model.UserBalance{UserID: u.ID, Balance: decimal.RequireFromString("12345678.87654321")}
	require.NoError(t, g.Create(&bal).Error)
	var got model.UserBalance
	require.NoError(t, g.First(&got, "user_id = ?", u.ID).Error)
	require.Equal(t, bal, got)
}

// Arrays and bytea round-trip through store.Strings and []byte; explicit
// false and 0 reach columns that default to true and 10.
func TestAccountRoundTrip(t *testing.T) {
	g := openGorm(t)
	acc := model.Account{
		Name: "a1", PluginKey: "p", Type: "t",
		CredentialsEnc: []byte{0, 1, 2, 0xff},
		Models:         store.Strings{"m1", "m2"},
	}
	require.NoError(t, g.Create(&acc).Error)
	require.Equal(t, ptr(true), acc.Schedulable)
	require.Equal(t, ptr(10), acc.Priority)
	require.Equal(t, store.Strings{}, acc.HeaderPassthrough)

	var got model.Account
	require.NoError(t, g.First(&got, acc.ID).Error)
	require.Equal(t, acc.CredentialsEnc, got.CredentialsEnc)
	require.Equal(t, store.Strings{"m1", "m2"}, got.Models)
	require.Nil(t, got.ParamOverride)

	off := model.Account{Name: "a2", PluginKey: "p", Type: "t", CredentialsEnc: []byte{0},
		Schedulable: ptr(false), Priority: ptr(0), AutoDisable: ptr(false)}
	require.NoError(t, g.Create(&off).Error)
	var back model.Account
	require.NoError(t, g.First(&back, off.ID).Error)
	require.Equal(t, ptr(false), back.Schedulable)
	require.Equal(t, ptr(0), back.Priority)
	require.Equal(t, ptr(false), back.AutoDisable)
}

// A -infinity default reads back as such, and created_at, which has no
// default, is stamped from the database clock.
func TestOutboxInfinityDefault(t *testing.T) {
	g := openGorm(t)
	row := model.ProviderHelperUsageOutbox{RequestID: "r1", AttemptID: "a", UserID: 1, GroupID: 1, Digest: "d", Payload: []byte("x")}
	require.NoError(t, g.Create(&row).Error)
	require.Equal(t, pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, row.NextAttemptAt)
	require.WithinDuration(t, time.Now(), row.CreatedAt, time.Minute)

	var got []model.ProviderHelperUsageOutbox
	require.NoError(t, g.Find(&got).Error)
	require.Len(t, got, 1)
	require.Equal(t, pgtype.NegativeInfinity, got[0].NextAttemptAt.InfinityModifier)
	require.True(t, row.CreatedAt.Equal(got[0].CreatedAt))
}

// The INSERT of a pointer to a zero value carries the column; a nil
// pointer leaves it to the default and reads it back.
func TestSnapshotPointerDefaults(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&model.Account{Name: "a", PluginKey: "p", Type: "t", CredentialsEnc: []byte{0},
			Schedulable: ptr(false), Priority: ptr(0)})
	})
}

// In a batch, a row whose pointer is nil gets DEFAULT where another row
// sets the column.
func TestSnapshotPointerDefaultsBatch(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&[]model.Account{
			{Name: "a", PluginKey: "p", Type: "t", CredentialsEnc: []byte{0}, Schedulable: ptr(false)},
			{Name: "b", PluginKey: "p", Type: "t", CredentialsEnc: []byte{0}},
		})
	})
}
