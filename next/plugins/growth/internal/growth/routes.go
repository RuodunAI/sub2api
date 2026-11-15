package growth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"

	"github.com/Sub2API-Devs/sup2api/next/plugins/growth/internal/model"
)

func (p *Plugin) routes() {
	// User routes
	p.Handle("GET", "/me/referral", p.getMyReferral)
	p.Handle("POST", "/me/referral/bind", p.bindReferralCode)
	p.Handle("GET", "/me/checkin/status", p.getCheckinStatus)
	p.Handle("POST", "/me/checkin", p.postCheckin)
	// Admin routes
	p.Handle("GET", "/admin/referrals", p.listReferrals)
	p.Handle("GET", "/admin/referrals/:user_id", p.getReferralDetail)
	p.Handle("GET", "/admin/commissions", p.listCommissions)
	p.Handle("GET", "/admin/checkins", p.listCheckins)
	p.Handle("GET", "/admin/stats", p.getStats)
}

func userID(req *pluginv1.HTTPRequest) int64 {
	if req.GetCaller() != nil {
		return req.GetCaller().GetUserId()
	}
	return 0
}

// unavailable is a 503 with the database error, the response every admin
// route of this plugin returns when its query fails.
func unavailable(err error) *pluginv1.HTTPResponse {
	return pluginsdk.Unavailable(err.Error())
}

// ---------------------------------------------------------------- /me/referral

type MyReferralResponse struct {
	Code              string          `json:"code"`
	InviterUserID     *int64          `json:"inviter_user_id"`
	BoundAt           *string         `json:"bound_at"`
	InviteesCount     int             `json:"invitees_count"`
	TotalCommission   decimal.Decimal `json:"total_commission"`
	TopInvitees       []TopInvitee    `json:"top_invitees"`
	RecentCommissions []Commission    `json:"recent_commissions"`
}

type TopInvitee struct {
	InviteeUserID int64           `json:"invitee_user_id"`
	Commission    decimal.Decimal `json:"commission"`
}

type Commission struct {
	ID            int64           `json:"id"`
	InviteeUserID int64           `json:"invitee_user_id"`
	EventType     string          `json:"event_type"`
	BaseAmount    decimal.Decimal `json:"base_amount"`
	Commission    decimal.Decimal `json:"commission"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (p *Plugin) getMyReferral(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid := userID(req)
	if uid == 0 {
		return pluginsdk.ErrorResponse(http.StatusUnauthorized, "unauthenticated", "login required"), nil
	}
	code, err := p.ensureCode(ctx, uid)
	if err != nil {
		return unavailable(err), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return unavailable(err), nil
	}
	var resp MyReferralResponse
	resp.Code = code
	// Load inviter. A user with no row yet is not an error (their referral
	// code exists but nobody bound them).
	var own model.ReferralCode
	err = db.Select("inviter_user_id, bound_at").Where("user_id = ?", uid).Take(&own).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return unavailable(err), nil
	}
	if own.InviterUserID != nil {
		resp.InviterUserID = own.InviterUserID
		if own.BoundAt != nil {
			s := own.BoundAt.Format(time.RFC3339)
			resp.BoundAt = &s
		}
	}
	// Count invitees and total commission.
	if resp.InviteesCount, err = count(referralCodes(db).Where("inviter_user_id = ?", uid)); err != nil {
		return unavailable(err), nil
	}
	if err := commissions(db).Select("COALESCE(SUM(commission), 0)").Where("inviter_user_id = ?", uid).Scan(&resp.TotalCommission).Error; err != nil {
		return unavailable(err), nil
	}
	// Top invitees.
	if err := commissions(db).Select("invitee_user_id, SUM(commission) AS commission").Where("inviter_user_id = ?", uid).
		Group("invitee_user_id").Order("SUM(commission) DESC").Limit(5).Find(&resp.TopInvitees).Error; err != nil {
		return unavailable(err), nil
	}
	resp.TopInvitees = orNil(resp.TopInvitees)
	// Recent commissions.
	if resp.RecentCommissions, err = recentCommissions(db, uid, 10); err != nil {
		return unavailable(err), nil
	}
	return pluginsdk.DataResponse(resp), nil
}

// count counts the rows q selects.
func count(q *gorm.DB) (int, error) {
	var n int64
	err := q.Count(&n).Error
	return int(n), err
}

// recentCommissions lists the latest commissions paid to inviterID, newest
// first, nil for none.
func recentCommissions(db *gorm.DB, inviterID int64, limit int) ([]Commission, error) {
	var out []Commission
	err := commissions(db).Select("id, invitee_user_id, event_type, base_amount, commission, created_at").
		Where("inviter_user_id = ?", inviterID).Order("created_at DESC").Limit(limit).Find(&out).Error
	return orNil(out), err
}

type BindRequest struct {
	Code string `json:"code"`
}

func (p *Plugin) bindReferralCode(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid := userID(req)
	if uid == 0 {
		return pluginsdk.ErrorResponse(http.StatusUnauthorized, "unauthenticated", "login required"), nil
	}
	var body BindRequest
	if err := pluginsdk.DecodeJSON(req, &body); err != nil {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", "invalid request body"), nil
	}
	if err := p.bindInviter(ctx, uid, body.Code); err != nil {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", err.Error()), nil
	}
	return pluginsdk.DataResponse(map[string]string{"status": "ok"}), nil
}

// ---------------------------------------------------------------- /me/checkin

type CheckinStatusResponse struct {
	Enabled         bool            `json:"enabled"`
	CheckedInToday  bool            `json:"checked_in_today"`
	TodayQuota      decimal.Decimal `json:"today_quota,omitempty"`
	ConsecutiveDays int             `json:"consecutive_days"`
	TotalCheckins   int             `json:"total_checkins"`
	TotalQuota      decimal.Decimal `json:"total_quota"`
	RecentCheckins  []Checkin       `json:"recent_checkins"`
}

type Checkin struct {
	CheckinDate  string          `json:"checkin_date"`
	QuotaAwarded decimal.Decimal `json:"quota_awarded"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (p *Plugin) getCheckinStatus(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid := userID(req)
	if uid == 0 {
		return pluginsdk.ErrorResponse(http.StatusUnauthorized, "unauthenticated", "login required"), nil
	}
	cfg := p.cfg.Load()
	resp := CheckinStatusResponse{Enabled: cfg.CheckinEnabled}
	if !cfg.CheckinEnabled {
		return pluginsdk.DataResponse(resp), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return unavailable(err), nil
	}
	now := time.Now().In(cfg.CheckinTimezone)
	today := now.Format("2006-01-02")
	// Check if checked in today. No row means "not yet", not a failure.
	var todays model.Checkin
	err = db.Select("quota_awarded").Where("user_id = ? AND checkin_date = ?", uid, today).Take(&todays).Error
	switch {
	case err == nil:
		resp.CheckedInToday = true
		resp.TodayQuota = todays.QuotaAwarded
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return unavailable(err), nil
	}
	// Total.
	if resp.TotalCheckins, resp.TotalQuota, err = checkinTotals(checkins(db).Where("user_id = ?", uid)); err != nil {
		return unavailable(err), nil
	}
	// Consecutive days.
	resp.ConsecutiveDays, err = p.countConsecutiveDays(db, uid, now)
	if err != nil {
		return unavailable(err), nil
	}
	// Recent.
	if err := checkins(db).Select(checkinDateText+", quota_awarded, created_at").Where("user_id = ?", uid).
		Order("checkin_date DESC").Limit(7).Find(&resp.RecentCheckins).Error; err != nil {
		return unavailable(err), nil
	}
	resp.RecentCheckins = orNil(resp.RecentCheckins)
	return pluginsdk.DataResponse(resp), nil
}

// checkinDateText selects checkin_date as YYYY-MM-DD: the driver reads a
// date as a time.Time, which database/sql would format as a timestamp into
// a string field.
const checkinDateText = "checkin_date::text AS checkin_date"

func (p *Plugin) countConsecutiveDays(db *gorm.DB, userID int64, now time.Time) (int, error) {
	// Count consecutive days backwards from yesterday (today doesn't count until tomorrow).
	date := now.AddDate(0, 0, -1)
	count := 0
	for i := 0; i < 365; i++ {
		ds := date.Format("2006-01-02")
		exists, err := checkedIn(db, userID, ds)
		if err != nil {
			return 0, err
		}
		if !exists {
			break
		}
		count++
		date = date.AddDate(0, 0, -1)
	}
	return count, nil
}

type CheckinPostResponse struct {
	QuotaAwarded decimal.Decimal `json:"quota_awarded"`
	CheckinDate  string          `json:"checkin_date"`
}

func (p *Plugin) postCheckin(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid := userID(req)
	if uid == 0 {
		return pluginsdk.ErrorResponse(http.StatusUnauthorized, "unauthenticated", "login required"), nil
	}
	result, err := p.doCheckin(ctx, uid)
	if err != nil {
		if strings.Contains(err.Error(), "not enabled") || strings.Contains(err.Error(), "already checked in") {
			return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", err.Error()), nil
		}
		return unavailable(err), nil
	}
	return pluginsdk.DataResponse(CheckinPostResponse{
		QuotaAwarded: result.QuotaAwarded,
		CheckinDate:  result.CheckinDate,
	}), nil
}

// ---------------------------------------------------------------- admin

// pagedList answers an admin list: it counts the rows from selects and reads
// the page the request asks for (columns, ordered by orderBy) into T. An
// empty page is null, as these endpoints have always answered.
func pagedList[T any](ctx context.Context, p *Plugin, req *pluginv1.HTTPRequest, from func(*gorm.DB) *gorm.DB, columns, orderBy string) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return unavailable(err), nil
	}
	page, size := pluginsdk.Pagination(req, 20)
	items, total, err := pgdb.List[T](from(db), columns, orderBy, size, (page-1)*size)
	if err != nil {
		return unavailable(err), nil
	}
	return pluginsdk.ListResponse(orNil(items), pluginsdk.Page{Page: page, PageSize: size, Total: total}), nil
}

func (p *Plugin) listReferrals(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	type Item struct {
		UserID        int64           `json:"user_id"`
		Code          string          `json:"code"`
		InviterUserID int64           `json:"inviter_user_id"`
		BoundAt       time.Time       `json:"bound_at"`
		Invitees      int             `json:"invitees"`
		Commission    decimal.Decimal `json:"commission"`
	}
	invited := func(db *gorm.DB) *gorm.DB {
		return db.Table("referral_codes r").Where("r.inviter_user_id IS NOT NULL")
	}
	return pagedList[Item](ctx, p, req, invited, `r.user_id, r.code, r.inviter_user_id, r.bound_at,
		(SELECT COUNT(*) FROM referral_codes WHERE inviter_user_id = r.user_id) AS invitees,
		COALESCE((SELECT SUM(commission) FROM commissions WHERE inviter_user_id = r.user_id), 0) AS commission`, "r.bound_at DESC")
}

func (p *Plugin) getReferralDetail(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uidStr := req.GetPathParams()["user_id"]
	if uidStr == "" {
		uidStr = strings.TrimPrefix(req.GetPath(), "/admin/referrals/")
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil || uid <= 0 {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", "invalid user_id"), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return unavailable(err), nil
	}
	type Detail struct {
		UserID        int64        `json:"user_id"`
		Code          string       `json:"code"`
		InviterUserID *int64       `json:"inviter_user_id"`
		BoundAt       *time.Time   `json:"bound_at"`
		Invitees      []int64      `json:"invitees"`
		Commissions   []Commission `json:"commissions"`
	}
	var row model.ReferralCode
	err = db.Select("code, inviter_user_id, bound_at").Where("user_id = ?", uid).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return pluginsdk.ErrorResponse(http.StatusNotFound, "not_found", "user not found"), nil
		}
		return unavailable(err), nil
	}
	d := Detail{UserID: uid, Code: row.Code, InviterUserID: row.InviterUserID, BoundAt: row.BoundAt}
	if err := referralCodes(db).Where("inviter_user_id = ?", uid).Order("bound_at DESC").Pluck("user_id", &d.Invitees).Error; err != nil {
		return unavailable(err), nil
	}
	d.Invitees = orNil(d.Invitees)
	if d.Commissions, err = recentCommissions(db, uid, 100); err != nil {
		return unavailable(err), nil
	}
	return pluginsdk.DataResponse(d), nil
}

func (p *Plugin) listCommissions(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	type Item struct {
		ID            int64           `json:"id"`
		InviterUserID int64           `json:"inviter_user_id"`
		InviteeUserID int64           `json:"invitee_user_id"`
		EventType     string          `json:"event_type"`
		BaseAmount    decimal.Decimal `json:"base_amount"`
		RatePercent   decimal.Decimal `json:"rate_percent"`
		Commission    decimal.Decimal `json:"commission"`
		CreatedAt     time.Time       `json:"created_at"`
	}
	return pagedList[Item](ctx, p, req, commissions,
		"id, inviter_user_id, invitee_user_id, event_type, base_amount, rate_percent, commission, created_at", "created_at DESC")
}

func (p *Plugin) listCheckins(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	type Item struct {
		ID           int64           `json:"id"`
		UserID       int64           `json:"user_id"`
		CheckinDate  string          `json:"checkin_date"`
		QuotaAwarded decimal.Decimal `json:"quota_awarded"`
		CreatedAt    time.Time       `json:"created_at"`
	}
	return pagedList[Item](ctx, p, req, checkins, "id, user_id, "+checkinDateText+", quota_awarded, created_at", "created_at DESC")
}

type StatsResponse struct {
	TotalReferrals    int             `json:"total_referrals"`
	TotalCommission   decimal.Decimal `json:"total_commission"`
	TotalCheckins     int             `json:"total_checkins"`
	TotalCheckinQuota decimal.Decimal `json:"total_checkin_quota"`
	TodayCheckins     int             `json:"today_checkins"`
}

func (p *Plugin) getStats(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return unavailable(err), nil
	}
	var s StatsResponse
	if s.TotalReferrals, err = count(referralCodes(db).Where("inviter_user_id IS NOT NULL")); err != nil {
		return unavailable(err), nil
	}
	if err := commissions(db).Select("COALESCE(SUM(commission), 0)").Scan(&s.TotalCommission).Error; err != nil {
		return unavailable(err), nil
	}
	if s.TotalCheckins, s.TotalCheckinQuota, err = checkinTotals(checkins(db)); err != nil {
		return unavailable(err), nil
	}
	cfg := p.cfg.Load()
	today := time.Now().In(cfg.CheckinTimezone).Format("2006-01-02")
	if s.TodayCheckins, err = count(checkins(db).Where("checkin_date = ?", today)); err != nil {
		return unavailable(err), nil
	}
	return pluginsdk.DataResponse(s), nil
}

// checkinTotals counts the check-ins q selects and sums their quota.
func checkinTotals(q *gorm.DB) (n int, quota decimal.Decimal, err error) {
	var t struct {
		N     int
		Quota decimal.Decimal
	}
	err = q.Select("COUNT(*) AS n, COALESCE(SUM(quota_awarded), 0) AS quota").Scan(&t).Error
	return t.N, t.Quota, err
}
