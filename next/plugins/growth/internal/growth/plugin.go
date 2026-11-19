// Package growth implements the growth & engagement plugin: referral
// commission and daily check-in. Referral codes are generated for all users
// (user.created event); invitees' usage.recorded and balance.changed events
// trigger commission credited via ledger.credit. Check-in awards random quota
// once per day per user.
package growth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/fieldcheck"

	"github.com/Sub2API-Devs/sup2api/next/plugins/growth/internal/model"
)

const JobCleanup = "cleanup"

// Plugin implements pluginsdk.App, pluginsdk.HTTP, pluginsdk.JobRunner,
// pluginsdk.SettingsValidator and lifecycle interfaces.
type Plugin struct {
	*pluginsdk.Router

	host pluginsdk.Host
	log  *slog.Logger
	cfg  atomic.Pointer[config]

	bg     context.Context
	cancel context.CancelFunc
}

type config struct {
	ReferralEnabled       bool
	ReferralRatePercent   decimal.Decimal
	ReferralMaxPerInvitee decimal.Decimal // 0 = unlimited
	ReferralDurationDays  int             // 0 = no expiry
	CheckinEnabled        bool
	CheckinMinQuota       decimal.Decimal
	CheckinMaxQuota       decimal.Decimal
	CheckinTimezone       *time.Location
}

func New() *Plugin {
	p := &Plugin{Router: pluginsdk.NewRouter()}
	p.bg, p.cancel = context.WithCancel(context.Background())
	c := defaultConfig()
	p.cfg.Store(&c)
	p.routes()
	return p
}

func (p *Plugin) Init(_ context.Context, h pluginsdk.Host) error {
	p.host = h
	p.log = h.Logger()
	return nil
}

func (p *Plugin) Configure(_ context.Context, cfg pluginsdk.Config) error {
	c, err := parseSettings(cfg)
	if err != nil {
		return err
	}
	p.cfg.Store(&c)
	return nil
}

// ValidateSettings implements pluginsdk.SettingsValidator: the console's
// settings are refused before they are stored when Configure would refuse
// them.
func (p *Plugin) ValidateSettings(_ context.Context, cfg pluginsdk.Config) (json.RawMessage, error) {
	_, err := parseSettings(cfg)
	return nil, err
}

func (p *Plugin) Shutdown(context.Context) error {
	p.cancel()
	return nil
}

// db returns the plugin schema's handle, bound to ctx (grant "db.schema").
func (p *Plugin) db(ctx context.Context) (*gorm.DB, error) {
	if p.host == nil {
		return nil, errors.New("growth: host not initialized")
	}
	db, err := p.host.DB(ctx)
	if err != nil {
		return nil, fmt.Errorf("growth: database unavailable: %w", err)
	}
	return db, nil
}

// ---------------------------------------------------------------- settings

type Settings struct {
	ReferralEnabled       bool    `json:"referral_enabled"`
	ReferralRatePercent   float64 `json:"referral_rate_percent"`
	ReferralMaxPerInvitee float64 `json:"referral_max_per_invitee"`
	ReferralDurationDays  int     `json:"referral_duration_days"`
	CheckinEnabled        bool    `json:"checkin_enabled"`
	CheckinMinQuota       float64 `json:"checkin_min_quota"`
	CheckinMaxQuota       float64 `json:"checkin_max_quota"`
	CheckinTimezone       string  `json:"checkin_timezone"`
}

func defaultConfig() config {
	return config{
		ReferralEnabled:       true,
		ReferralRatePercent:   decimal.NewFromInt(10),
		ReferralMaxPerInvitee: decimal.NewFromInt(100),
		ReferralDurationDays:  0,
		CheckinEnabled:        true,
		CheckinMinQuota:       decimal.NewFromFloat(0.01),
		CheckinMaxQuota:       decimal.NewFromFloat(0.05),
		CheckinTimezone:       time.UTC,
	}
}

// settingsRules are the settings the console form edits (ui/native
// GrowthSettings shows the same bounds); every other key is refused.
var settingsRules = fieldcheck.Fields{
	"referral_enabled":         fieldcheck.Bool(),
	"referral_rate_percent":    fieldcheck.Number().Range(0, 100),
	"referral_max_per_invitee": fieldcheck.Number().Min(0),
	"referral_duration_days":   fieldcheck.Integer().Min(0),
	"checkin_enabled":          fieldcheck.Bool(),
	"checkin_min_quota":        fieldcheck.Number().Min(0),
	"checkin_max_quota":        fieldcheck.Number().Min(0),
	"checkin_timezone":         fieldcheck.String(),
}

// parseSettings decodes and checks the settings: the one check of Configure
// and ValidateSettings.
func parseSettings(cfg pluginsdk.Config) (config, error) {
	invalid := pluginsdk.FieldErrors{}.Add("", "invalid_json", "settings must be a JSON object").Err()
	var values map[string]any
	if err := cfg.Decode(&values); err != nil {
		return config{}, invalid
	}
	if err := fieldcheck.Check(settingsRules, values).Err(); err != nil {
		return config{}, err
	}
	var s Settings
	if err := cfg.Decode(&s); err != nil {
		return config{}, invalid
	}
	c, errs := compileSettings(s)
	return c, errs.Err()
}

// compileSettings turns settings that passed settingsRules into the config:
// the checks across fields and of the time zone.
func compileSettings(s Settings) (config, pluginsdk.FieldErrors) {
	var errs pluginsdk.FieldErrors
	c := config{
		ReferralEnabled:       s.ReferralEnabled,
		ReferralRatePercent:   decimal.NewFromFloat(s.ReferralRatePercent),
		ReferralMaxPerInvitee: decimal.NewFromFloat(s.ReferralMaxPerInvitee),
		ReferralDurationDays:  s.ReferralDurationDays,
		CheckinEnabled:        s.CheckinEnabled,
		CheckinMinQuota:       decimal.NewFromFloat(s.CheckinMinQuota),
		CheckinMaxQuota:       decimal.NewFromFloat(s.CheckinMaxQuota),
	}
	if c.CheckinMaxQuota.LessThan(c.CheckinMinQuota) {
		errs = errs.Add("checkin_max_quota", "invalid", "must be >= checkin_min_quota")
	}
	tz := strings.TrimSpace(s.CheckinTimezone)
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		errs = errs.Add("checkin_timezone", "invalid", "unknown timezone")
	} else {
		c.CheckinTimezone = loc
	}
	return c, errs
}

// ---------------------------------------------------------------- referral codes

// generateCode returns a random 8-character uppercase alphanumeric code.
func generateCode() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:8], nil
}

// ensureCode creates a referral code for userID if it doesn't exist.
func (p *Plugin) ensureCode(ctx context.Context, userID int64) (string, error) {
	db, err := p.db(ctx)
	if err != nil {
		return "", err
	}
	code, err := codeOf(db, userID)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	// Generate code and insert (conflict: query again).
	for i := 0; i < 10; i++ {
		code, err = generateCode()
		if err != nil {
			return "", err
		}
		res := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoNothing: true}).
			Create(&model.ReferralCode{UserID: userID, Code: code})
		// The user_id conflict is absorbed: a duplicate key is the code's.
		if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
			continue // collision, retry
		}
		if res.Error != nil {
			return "", res.Error
		}
		if res.RowsAffected == 0 {
			// A concurrent call stored the user's code first: answer that
			// one, not the code generated here.
			return codeOf(db, userID)
		}
		return code, nil
	}
	// Fallback: query existing.
	return codeOf(db, userID)
}

// codeOf reads the referral code of userID (gorm.ErrRecordNotFound: none).
func codeOf(db *gorm.DB, userID int64) (string, error) {
	var row model.ReferralCode
	err := db.Select("code").Where("user_id = ?", userID).Take(&row).Error
	return row.Code, err
}

// referralCodes selects from referral_codes.
func referralCodes(db *gorm.DB) *gorm.DB { return db.Model(&model.ReferralCode{}) }

// bindInviter sets the inviter for userID (idempotent, only works when inviter_user_id is NULL).
func (p *Plugin) bindInviter(ctx context.Context, userID int64, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || len(code) > 32 {
		return errors.New("invalid code")
	}
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	var inviter model.ReferralCode
	err = db.Select("user_id, inviter_user_id").Where("code = ?", code).Take(&inviter).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("code not found")
		}
		return err
	}
	inviterID, inviterInviter := inviter.UserID, inviter.InviterUserID
	if inviterID == userID {
		return errors.New("cannot refer yourself")
	}
	// Two users inviting each other would pay each other commission.
	if inviterInviter != nil && *inviterInviter == userID {
		return errors.New("cannot bind to a user you invited")
	}
	// Users created before the plugin was enabled have no row until they
	// open their referral page; without one the update below matches nothing.
	if _, err := p.ensureCode(ctx, userID); err != nil {
		return err
	}
	res := referralCodes(db).Where("user_id = ? AND inviter_user_id IS NULL", userID).
		Updates(map[string]any{"inviter_user_id": inviterID, "bound_at": gorm.Expr("now()")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("already bound")
	}
	return nil
}

// ---------------------------------------------------------------- commission

// recordCommission credits commission to inviter if conditions are met.
// Idempotent on eventType+eventID.
func (p *Plugin) recordCommission(ctx context.Context, inviteeUserID int64, eventType string, eventID int64, baseAmount decimal.Decimal) error {
	if inviteeUserID <= 0 || eventID <= 0 || baseAmount.Sign() <= 0 {
		return nil
	}
	cfg := p.cfg.Load()
	if !cfg.ReferralEnabled || cfg.ReferralRatePercent.Sign() <= 0 {
		return nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	// Check if already processed.
	done, err := pgdb.Exists(db, commissions(db).Where("event_type = ? AND event_id = ?", eventType, eventID))
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	// Load inviter and check duration.
	var invitee model.ReferralCode
	err = db.Select("inviter_user_id, bound_at").Where("user_id = ? AND inviter_user_id IS NOT NULL", inviteeUserID).Take(&invitee).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // not invited
		}
		return err
	}
	inviterID := *invitee.InviterUserID // not NULL: the WHERE above
	// bindInviter sets bound_at with the inviter. A row without one is
	// broken: fail, so that the event is delivered again, rather than pay or
	// refuse a commission on a binding time nobody knows.
	if invitee.BoundAt == nil {
		return fmt.Errorf("growth: referral of user %d has an inviter but no bound_at", inviteeUserID)
	}
	if cfg.ReferralDurationDays > 0 {
		if time.Now().After(invitee.BoundAt.AddDate(0, 0, cfg.ReferralDurationDays)) {
			return nil // expired
		}
	}
	// Per-invitee cap: read the credited total once and reuse it for both the
	// "already at the cap" check and the clamp below.
	var credited decimal.Decimal
	capped := cfg.ReferralMaxPerInvitee.Sign() > 0
	if capped {
		err = commissions(db).Select("COALESCE(SUM(commission), 0)").
			Where("inviter_user_id = ? AND invitee_user_id = ?", inviterID, inviteeUserID).Scan(&credited).Error
		if err != nil {
			return err
		}
		if credited.GreaterThanOrEqual(cfg.ReferralMaxPerInvitee) {
			return nil // cap reached
		}
	}
	commission := baseAmount.Mul(cfg.ReferralRatePercent).Div(decimal.NewFromInt(100)).Round(8)
	if commission.Sign() <= 0 {
		return nil
	}
	// Apply cap.
	if capped {
		remaining := cfg.ReferralMaxPerInvitee.Sub(credited)
		if commission.GreaterThan(remaining) {
			commission = remaining.Round(8)
		}
		if commission.Sign() <= 0 {
			return nil
		}
	}
	// Credit via ledger.
	idemKey := fmt.Sprintf("commission:%s:%d", eventType, eventID)
	res, err := p.host.LedgerCredit(ctx, pluginsdk.LedgerChange{
		UserID:         inviterID,
		Amount:         commission.StringFixed(8),
		IdempotencyKey: idemKey,
		RefType:        "referral",
		RefID:          fmt.Sprintf("%d", eventID),
		Note:           fmt.Sprintf("Referral commission from user %d (%s %d)", inviteeUserID, eventType, eventID),
	})
	if err != nil {
		return err
	}
	// Record commission. A duplicate means an earlier attempt credited the
	// ledger and failed before this insert: record it now, or the
	// per-invitee cap would not count it.
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "ledger_id"}}, DoNothing: true}).Create(&model.Commission{
		InviterUserID: inviterID, InviteeUserID: inviteeUserID, EventType: eventType, EventID: eventID,
		BaseAmount: baseAmount, RatePercent: cfg.ReferralRatePercent, Commission: commission, LedgerID: res.LedgerID,
	}).Error
}

// commissions selects from commissions.
func commissions(db *gorm.DB) *gorm.DB { return db.Model(&model.Commission{}) }

// ---------------------------------------------------------------- check-in

// doCheckin performs daily check-in for userID.
func (p *Plugin) doCheckin(ctx context.Context, userID int64) (*CheckinResult, error) {
	cfg := p.cfg.Load()
	if !cfg.CheckinEnabled {
		return nil, errors.New("check-in not enabled")
	}
	db, err := p.db(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(cfg.CheckinTimezone)
	today := now.Format("2006-01-02")
	// Check if already checked in today.
	done, err := checkedIn(db, userID, today)
	if err != nil {
		return nil, err
	}
	if done {
		return nil, errors.New("already checked in today")
	}
	// Random quota between min and max.
	quota := cfg.CheckinMinQuota
	if cfg.CheckinMaxQuota.GreaterThan(cfg.CheckinMinQuota) {
		diff := cfg.CheckinMaxQuota.Sub(cfg.CheckinMinQuota)
		diffFloat, _ := diff.Float64()
		randFloat := randFloat64() * diffFloat
		quota = quota.Add(decimal.NewFromFloat(randFloat)).Round(8)
	}
	if quota.Sign() <= 0 {
		return nil, errors.New("invalid quota range")
	}
	// Credit via ledger.
	idemKey := fmt.Sprintf("checkin:%d:%s", userID, today)
	res, err := p.host.LedgerCredit(ctx, pluginsdk.LedgerChange{
		UserID:         userID,
		Amount:         quota.StringFixed(8),
		IdempotencyKey: idemKey,
		RefType:        "checkin",
		RefID:          today,
		Note:           fmt.Sprintf("Daily check-in %s", today),
	})
	if err != nil {
		return nil, err
	}
	if res.Duplicate {
		return nil, errors.New("already checked in today")
	}
	// Record check-in.
	ins := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "checkin_date"}}, DoNothing: true}).
		Create(&model.Checkin{UserID: userID, CheckinDate: today, QuotaAwarded: quota, LedgerID: res.LedgerID})
	if ins.Error != nil {
		return nil, ins.Error
	}
	if ins.RowsAffected == 0 {
		return nil, errors.New("already checked in today")
	}
	return &CheckinResult{QuotaAwarded: quota, CheckinDate: today}, nil
}

// checkins selects from checkins.
func checkins(db *gorm.DB) *gorm.DB { return db.Model(&model.Checkin{}) }

// checkedIn reports whether userID checked in on date (YYYY-MM-DD).
func checkedIn(db *gorm.DB, userID int64, date string) (bool, error) {
	return pgdb.Exists(db, checkins(db).Where("user_id = ? AND checkin_date = ?", userID, date))
}

// orNil keeps an empty list nil: these endpoints have always answered null,
// not [], when there is nothing to show, and GORM reads an empty result as
// an empty slice.
func orNil[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

type CheckinResult struct {
	QuotaAwarded decimal.Decimal
	CheckinDate  string
}

func randFloat64() float64 {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<53))
	return float64(n.Int64()) / float64(1<<53)
}
