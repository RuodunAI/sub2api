// Package model is the schema of plg_payment as GORM models: the one
// description of its tables. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
//
// Nullable columns are pointers. The association fields only name the
// foreign keys (fk_<table>_<field>); nothing loads or saves through them.
//
// GORM writes a column's parsed default (default:'balance', default:0)
// in place of a zero value on Create, where a plain INSERT writes the zero
// value: rows whose zero values must reach the database as such are
// inserted with SQL (see the services).
package model

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// All lists every model, for sub2api-plugin db.
func All() []any {
	return []any{&ProviderInstance{}, &Order{}, &AuditLog{}, &RedeemCode{}, &PromoCode{}, &PromoCodeUsage{}, &Config{}}
}

// ProviderInstance is a configured payment channel.
type ProviderInstance struct {
	ID              int64           `gorm:"primaryKey"`
	ProviderKey     string          `gorm:"type:varchar(50);not null;index:idx_payment_provider_instances_provider_key"`
	Name            string          `gorm:"type:varchar(100);not null"`
	Config          json.RawMessage `gorm:"type:jsonb;not null;default:'{}'"`
	Enabled         bool            `gorm:"not null;default:true;index:idx_payment_provider_instances_enabled,priority:1"`
	SortOrder       int             `gorm:"type:integer;not null;default:0;index:idx_payment_provider_instances_enabled,priority:2"`
	Limits          json.RawMessage `gorm:"type:jsonb"`
	RefundEnabled   bool            `gorm:"not null;default:false"`
	AllowUserRefund bool            `gorm:"not null;default:false"`
	CreatedAt       time.Time       `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt       time.Time       `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (ProviderInstance) TableName() string { return "payment_provider_instances" }

// Order is a recharge order.
type Order struct {
	ID                 int64             `gorm:"primaryKey"`
	UserID             int64             `gorm:"not null;index:idx_payment_orders_user_id,priority:1"`
	UserEmail          string            `gorm:"type:varchar(255);not null"`
	OutTradeNo         string            `gorm:"type:varchar(100);not null;unique;index:idx_payment_orders_out_trade_no"`
	PaymentType        string            `gorm:"type:varchar(50);not null"`
	PaymentTradeNo     *string           `gorm:"type:varchar(200);index:idx_payment_orders_payment_trade_no,where:payment_trade_no IS NOT NULL"` // nil until paid
	ProviderInstanceID *int64            `gorm:"type:bigint"`
	ProviderInstance   *ProviderInstance `gorm:"constraint:OnDelete:SET NULL"`
	ProviderKey        *string           `gorm:"type:varchar(50)"`
	ProviderSnapshot   json.RawMessage   `gorm:"type:jsonb"`
	Amount             decimal.Decimal   `gorm:"type:numeric(20,8);not null"`
	PayAmount          decimal.Decimal   `gorm:"type:numeric(20,8);not null"`
	FeeRate            decimal.Decimal   `gorm:"type:numeric(10,4);not null;default:0"`
	Currency           string            `gorm:"type:varchar(10);not null;default:'CNY'"`
	OrderType          string            `gorm:"type:varchar(50);not null;default:'balance'"`
	Status             string            `gorm:"type:varchar(50);not null;default:'pending';index:idx_payment_orders_status,priority:1"`
	ExpiresAt          time.Time         `gorm:"not null;index:idx_payment_orders_status,priority:2"`
	PaidAt             *time.Time
	FailedAt           *time.Time
	FailedReason       *string
	ClientIP           *string `gorm:"type:varchar(100)"`
	SrcHost            *string `gorm:"type:varchar(255)"`
	SrcURL             *string
	RechargeCode       *string `gorm:"type:varchar(100);index:idx_payment_orders_recharge_code,where:recharge_code IS NOT NULL"`
	PlanID             *int64  `gorm:"type:bigint"`
	UserNotes          *string
	CreatedAt          time.Time `gorm:"not null;default:now();autoCreateTime:false;index:idx_payment_orders_user_id,priority:2,sort:desc"`
	UpdatedAt          time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (Order) TableName() string { return "payment_orders" }

// AuditLog is an event of an order.
type AuditLog struct {
	ID          int64           `gorm:"primaryKey"`
	OrderID     int64           `gorm:"not null;index:idx_payment_audit_logs_order_id,priority:1"`
	Order       *Order          `gorm:"constraint:OnDelete:CASCADE"`
	EventType   string          `gorm:"type:varchar(100);not null"`
	ProviderKey *string         `gorm:"type:varchar(50)"`
	Details     json.RawMessage `gorm:"type:jsonb"`
	CreatedAt   time.Time       `gorm:"not null;default:now();autoCreateTime:false;index:idx_payment_audit_logs_order_id,priority:2"`
}

func (AuditLog) TableName() string { return "payment_audit_logs" }

// RedeemCode is a redeem code; CodeHash (SHA-256 of Code) is what users
// redeem by.
type RedeemCode struct {
	ID           int64           `gorm:"primaryKey"`
	Code         string          `gorm:"type:varchar(100);not null;unique"`
	CodeHash     string          `gorm:"type:varchar(128);not null;unique;index:idx_redeem_codes_code_hash"`
	Type         string          `gorm:"type:varchar(50);not null;default:'balance'"`
	Value        decimal.Decimal `gorm:"type:numeric(20,8);not null"`
	GroupID      *int64          `gorm:"type:bigint"`
	ValidityDays *int            `gorm:"type:integer"`
	Status       string          `gorm:"type:varchar(50);not null;default:'unused';index:idx_redeem_codes_status,priority:1"`
	UsedBy       *int64          `gorm:"type:bigint;index:idx_redeem_codes_used_by,where:used_by IS NOT NULL"`
	UsedAt       *time.Time
	ExpiresAt    *time.Time `gorm:"index:idx_redeem_codes_status,priority:2"`
	Notes        *string
	CreatedAt    time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt    time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (RedeemCode) TableName() string { return "redeem_codes" }

// PromoCode is a promotional code that credits a bonus once per user.
type PromoCode struct {
	ID          int64           `gorm:"primaryKey"`
	Code        string          `gorm:"type:varchar(100);not null;unique;index:idx_promo_codes_code"`
	BonusAmount decimal.Decimal `gorm:"type:numeric(20,8);not null"`
	MaxUses     int             `gorm:"type:integer;not null;default:0"` // 0: unlimited
	UsedCount   int             `gorm:"type:integer;not null;default:0"`
	Status      string          `gorm:"type:varchar(50);not null;default:'active';index:idx_promo_codes_status,priority:1"`
	ExpiresAt   *time.Time      `gorm:"index:idx_promo_codes_status,priority:2"`
	Notes       *string
	CreatedAt   time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt   time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (PromoCode) TableName() string { return "promo_codes" }

// PromoCodeUsage records that a user applied a promo code (once each).
type PromoCodeUsage struct {
	ID          int64           `gorm:"primaryKey"`
	PromoCodeID int64           `gorm:"not null;uniqueIndex:promo_code_usage_promo_code_id_user_id_key,priority:1;index:idx_promo_code_usage_promo_code_id"`
	PromoCode   *PromoCode      `gorm:"constraint:OnDelete:CASCADE"`
	UserID      int64           `gorm:"not null;uniqueIndex:promo_code_usage_promo_code_id_user_id_key,priority:2;index:idx_promo_code_usage_user_id,priority:1"`
	BonusAmount decimal.Decimal `gorm:"type:numeric(20,8);not null"`
	UsedAt      time.Time       `gorm:"not null;default:now();index:idx_promo_code_usage_user_id,priority:2,sort:desc"`
}

func (PromoCodeUsage) TableName() string { return "promo_code_usage" }

// ConfigID is the id of the one configuration row (the migration inserts it).
const ConfigID = 1

// Config is the payment configuration, one row.
type Config struct {
	ID                  int             `gorm:"type:integer;primaryKey;autoIncrement:false;default:1;check:payment_config_id_check,id = 1"`
	Enabled             bool            `gorm:"not null;default:true"`
	MinAmount           decimal.Decimal `gorm:"type:numeric(20,8);not null;default:1"`
	MaxAmount           decimal.Decimal `gorm:"type:numeric(20,8);not null;default:10000"`
	DailyLimit          decimal.Decimal `gorm:"type:numeric(20,8);not null;default:0"`
	OrderTimeoutMinutes int             `gorm:"type:integer;not null;default:30"`
	MaxPendingOrders    int             `gorm:"type:integer;not null;default:3"`
	// The default is spelt the way PostgreSQL stores it, so that it compares
	// equal (the driver cuts a stored default at its first cast).
	EnabledPaymentTypes       pgdb.Array[string] `gorm:"type:text[];not null;default:ARRAY['alipay'::text, 'wxpay'::text]"`
	BalanceDisabled           bool               `gorm:"not null;default:false"`
	BalanceRechargeMultiplier decimal.Decimal    `gorm:"type:numeric(10,4);not null;default:1.0"`
	RechargeFeeRate           decimal.Decimal    `gorm:"type:numeric(10,4);not null;default:0"`
	LoadBalanceStrategy       string             `gorm:"type:varchar(50);not null;default:'round-robin'"`
	Settings                  json.RawMessage    `gorm:"type:jsonb;not null;default:'{}'"`
	CreatedAt                 time.Time          `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt                 time.Time          `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (Config) TableName() string { return "payment_config" }
