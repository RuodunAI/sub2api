package model_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// These tests check the models against a migrated database through
// store.G, the path production code takes.
func openGorm(t *testing.T) *gorm.DB {
	t.Helper()
	return store.G(context.Background(), testutil.DB(t).Pool)
}

func dryRun(g *gorm.DB) *gorm.DB { return g.Session(&gorm.Session{DryRun: true}) }

// A serial id and every defaulted column are left to the database and read
// back on insert; nullable columns round-trip nil and values.
func TestUserRoundTrip(t *testing.T) {
	g := openGorm(t)
	u := model.User{Email: "a@example.com", PasswordHash: "h"}
	require.NoError(t, g.Create(&u).Error)
	require.NotZero(t, u.ID)
	require.Equal(t, "active", u.Status)
	require.Equal(t, 5, u.MaxConcurrency)
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
	require.Equal(t, u.UpdatedAt, again.UpdatedAt, "GORM must not stamp updated_at")

	// A zero value in a defaulted column means "database default", even when
	// selected; a map insert writes the zero.
	z := model.User{Email: "z@example.com", PasswordHash: "h"}
	require.NoError(t, g.Select("Email", "PasswordHash", "MaxConcurrency").Create(&z).Error)
	require.Equal(t, 5, z.MaxConcurrency)
	require.NoError(t, g.Model(&model.User{}).Create(map[string]any{
		"email": "m@example.com", "password_hash": "h", "max_concurrency": 0,
	}).Error)
	var m model.User
	require.NoError(t, g.First(&m, "email = ?", "m@example.com").Error)
	require.Equal(t, 0, m.MaxConcurrency)
}

// numeric keeps every digit, jsonb round-trips as raw JSON, and GORM adds
// no updated_at of its own to an update.
func TestGroupRoundTrip(t *testing.T) {
	g := openGorm(t)
	policy := "strict"
	grp := model.Group{
		Name:                 "g1",
		RateMultiplier:       decimal.RequireFromString("1.2345"),
		ModelAllowlist:       json.RawMessage(`["claude-x", "gpt-y"]`),
		ConversionLossPolicy: &policy,
	}
	require.NoError(t, g.Create(&grp).Error)
	require.Equal(t, "1.2345", grp.RateMultiplier.String())
	require.JSONEq(t, `{}`, string(grp.Environment))
	require.Equal(t, "blacklist", grp.ModelFilterMode)

	var got model.Group
	require.NoError(t, g.First(&got, grp.ID).Error)
	require.Equal(t, grp, got)
	require.JSONEq(t, `["claude-x","gpt-y"]`, string(got.ModelAllowlist))

	plain := model.Group{Name: "g2"}
	require.NoError(t, g.Create(&plain).Error)
	require.True(t, plain.RateMultiplier.Equal(decimal.NewFromInt(1)))
	require.JSONEq(t, `[]`, string(plain.ModelAllowlist))
	require.Nil(t, plain.ConversionLossPolicy)

	stmt := dryRun(g).Model(&model.Group{ID: grp.ID}).Update("description", "x").Statement
	require.Equal(t, `UPDATE "groups" SET "description"=$1 WHERE "id" = $2`, stmt.SQL.String())
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
	require.Equal(t, 1, log.Attempts)
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
	stmt := dryRun(g).Create(&model.UserBalance{}).Statement
	require.Contains(t, stmt.SQL.String(), `"user_id"`)

	u := model.User{Email: "b@example.com", PasswordHash: "h"}
	require.NoError(t, g.Create(&u).Error)
	bal := model.UserBalance{UserID: u.ID, Balance: decimal.RequireFromString("12345678.87654321")}
	require.NoError(t, g.Create(&bal).Error)
	var got model.UserBalance
	require.NoError(t, g.First(&got, "user_id = ?", u.ID).Error)
	require.Equal(t, bal, got)
}

// Arrays and bytea round-trip through store.Strings and []byte.
func TestAccountRoundTrip(t *testing.T) {
	g := openGorm(t)
	acc := model.Account{
		Name: "a1", PluginKey: "p", Type: "t",
		CredentialsEnc: []byte{0, 1, 2, 0xff},
		Models:         store.Strings{"m1", "m2"},
	}
	require.NoError(t, g.Create(&acc).Error)
	require.True(t, acc.Schedulable)
	require.Equal(t, store.Strings{}, acc.HeaderPassthrough)

	var got model.Account
	require.NoError(t, g.First(&got, acc.ID).Error)
	require.Equal(t, acc.CredentialsEnc, got.CredentialsEnc)
	require.Equal(t, store.Strings{"m1", "m2"}, got.Models)
	require.Nil(t, got.ParamOverride)
}
