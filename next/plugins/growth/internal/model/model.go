// Package model is the schema of plg_growth as GORM models: the one
// description of its tables. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// All lists every model, for sub2api-plugin db.
func All() []any {
	return []any{&ReferralCode{}, &Commission{}, &Checkin{}}
}

// ReferralCode is a user's referral code and, once bound, who invited them.
type ReferralCode struct {
	UserID        int64      `gorm:"primaryKey;autoIncrement:false"`
	Code          string     `gorm:"not null;unique"`
	InviterUserID *int64     `gorm:"index:referral_codes_inviter_idx,where:inviter_user_id IS NOT NULL"` // nil: not invited
	CreatedAt     time.Time  `gorm:"not null;default:now();autoCreateTime:false"`
	BoundAt       *time.Time // when the inviter was bound
}

func (ReferralCode) TableName() string { return "referral_codes" }

// Commission is one credited commission, idempotent on its ledger entry.
type Commission struct {
	ID            int64           `gorm:"primaryKey"`
	InviterUserID int64           `gorm:"not null;index:commissions_inviter_created_idx,priority:1"`
	InviteeUserID int64           `gorm:"not null;index:commissions_invitee_idx"`
	EventType     string          `gorm:"not null;index:commissions_event_idx,priority:1"` // usage.recorded | balance.changed
	EventID       int64           `gorm:"not null;index:commissions_event_idx,priority:2"` // usage_log id or balance_ledger id
	BaseAmount    decimal.Decimal `gorm:"type:numeric(20,8);not null"`                     // usage cost or recharge amount
	RatePercent   decimal.Decimal `gorm:"type:numeric(5,2);not null"`
	Commission    decimal.Decimal `gorm:"type:numeric(20,8);not null"`
	LedgerID      int64           `gorm:"not null;unique"` // balance_ledger.id
	CreatedAt     time.Time       `gorm:"not null;default:now();autoCreateTime:false;index:commissions_inviter_created_idx,priority:2,sort:desc"`
}

func (Commission) TableName() string { return "commissions" }

// Checkin is one user's check-in of one day.
type Checkin struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64 `gorm:"not null;uniqueIndex:checkins_user_id_checkin_date_key,priority:1;index:checkins_user_date_idx,priority:1"`
	// CheckinDate is the date in the configured time zone, written as
	// YYYY-MM-DD. The driver reads a date back as a time.Time, which
	// database/sql would format as a timestamp into a string: reads select
	// checkin_date::text.
	CheckinDate  string          `gorm:"type:date;not null;uniqueIndex:checkins_user_id_checkin_date_key,priority:2;index:checkins_user_date_idx,priority:2,sort:desc;index:checkins_date_idx"`
	QuotaAwarded decimal.Decimal `gorm:"type:numeric(20,8);not null"`
	LedgerID     int64           `gorm:"not null;unique"`
	CreatedAt    time.Time       `gorm:"not null;default:now();autoCreateTime:false"`
}

func (Checkin) TableName() string { return "checkins" }
