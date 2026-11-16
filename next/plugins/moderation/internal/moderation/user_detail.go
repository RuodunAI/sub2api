package moderation

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// GET /users/:user_id is the user detail drawer (new-api
// GetModerationWatchDetail): it answers for any user id, on the watch list
// or not. The username, role and status come from the core (/users/:id),
// which the console reads itself; the timeline and the watch history are
// GET /events?user_id= and GET /watchlist/:user_id/events.

// UserSummary counts all suspicious records of a user.
type UserSummary struct {
	Total         int64          `json:"total"`
	Pending       int64          `json:"pending"`
	Confirmed     int64          `json:"confirmed"`
	FalsePositive int64          `json:"false_positive"`
	BySeverity    SeverityCounts `json:"by_severity"`
}

// ContextSource is one "group → flow@version" the user's records of the
// last recentDays came through.
type ContextSource struct {
	GroupID  int64  `json:"group_id"`
	FlowID   int64  `json:"flow_id"`
	FlowName string `json:"flow_name"`
	Version  int    `json:"version"`
	Count    int64  `json:"count"`
	Current  bool   `json:"current"`
}

// UserContext is the context bar: the group of the latest record, the flow
// bound to it now and the recent sources.
type UserContext struct {
	GroupID *int64          `json:"group_id"`
	Flow    *FlowRef        `json:"flow"`
	Sources []ContextSource `json:"sources"`
}

// UserDay is one user_daily row.
type UserDay struct {
	Day        string     `json:"day"` // UTC, 2006-01-02
	GroupID    int64      `json:"group_id"`
	Audited    int64      `json:"audited"`
	Suspicious int64      `json:"suspicious"`
	High       int64      `json:"high"`
	Errors     int64      `json:"errors"`
	LastHitAt  *time.Time `json:"last_hit_at"`
}

// UserDetail is GET /users/:user_id.
type UserDetail struct {
	UserID int64 `json:"user_id"`
	UserState
	Summary UserSummary `json:"summary"`
	Context UserContext `json:"context"`
	Daily   []UserDay   `json:"daily"`
}

func (p *Plugin) getUserDetail(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	now := p.now().UTC()
	d := &UserDetail{UserID: uid}
	if d.UserState, err = p.userState(ctx, db, uid); err != nil {
		return nil, err
	}
	for _, load := range []func(context.Context, *gorm.DB, *UserDetail, time.Time) error{
		loadUserSummary, p.loadUserContext, p.loadUserDaily,
	} {
		if err := load(ctx, db, d, now); err != nil {
			return nil, err
		}
	}
	return pluginsdk.DataResponse(d), nil
}

func loadUserSummary(ctx context.Context, db *gorm.DB, d *UserDetail, _ time.Time) error {
	var r struct{ Total, High, Medium, Low, Pending, Confirmed, FalsePositive int64 }
	err := db.Model(&model.Event{}).Select(severityCounts+`, count(*) FILTER (WHERE review_status = 'pending') AS pending,
			count(*) FILTER (WHERE review_status = 'confirmed') AS confirmed,
			count(*) FILTER (WHERE review_status = 'false_positive') AS false_positive`).
		Where("user_id = ? AND "+suspiciousResult, d.UserID).Scan(&r).Error
	d.Summary = UserSummary{Total: r.Total, Pending: r.Pending, Confirmed: r.Confirmed, FalsePositive: r.FalsePositive,
		BySeverity: SeverityCounts{High: r.High, Medium: r.Medium, Low: r.Low}}
	return err
}

// loadUserContext: the group of the latest record (the plugin cannot read
// the user's group from the core), its flow now, and the suspicious records
// of the last recentDays by group, flow and version.
func (p *Plugin) loadUserContext(ctx context.Context, db *gorm.DB, d *UserDetail, now time.Time) error {
	uc := &d.Context
	var group int64
	err := db.Model(&model.Event{}).Select("group_id").Where("user_id = ?", d.UserID).Order("created_at DESC").Take(&group).Error
	switch {
	case err == nil:
		uc.GroupID = &group
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return err
	}
	snap := p.snap.Load()
	if f, _ := snap.flowFor(group); uc.GroupID != nil && f != nil {
		uc.Flow = &FlowRef{FlowID: f.ID, Name: f.Name, Version: f.Version}
	}
	uc.Sources, err = p.loadSources(db, d.UserID, now.Add(-recentDays*24*time.Hour), uc.GroupID, uc.Flow)
	return err
}

// loadSources returns the user's suspicious records since `since` by group,
// flow and version ("group → flow@version"), latest first, at most 50.
// current marks the source of group through flow, the
// flow bound to that group now (both may be nil).
func (p *Plugin) loadSources(db *gorm.DB, uid int64, since time.Time, group *int64, flow *FlowRef) ([]ContextSource, error) {
	names, err := p.flowNames(db)
	if err != nil {
		return nil, err
	}
	sources := []ContextSource{}
	err = db.Model(&model.Event{}).Select("group_id, flow_id, flow_version AS version, count(*) AS count").
		Where("user_id = ? AND created_at >= ? AND "+suspiciousResult, uid, since).
		Group("group_id, flow_id, flow_version").Order("max(created_at) DESC").Limit(50).Scan(&sources).Error
	for i := range sources {
		s := &sources[i]
		s.FlowName = names[s.FlowID]
		s.Current = group != nil && flow != nil && s.GroupID == *group && s.FlowID == flow.FlowID && s.Version == flow.Version
	}
	return sources, err
}

// loadUserDaily reads the user's user_daily rows, this node's pending
// counts included.
func (p *Plugin) loadUserDaily(ctx context.Context, db *gorm.DB, d *UserDetail, now time.Time) error {
	p.flushStats(ctx)
	var rows []model.UserDaily
	err := db.Select("day, group_id, audited, suspicious, high, errors, last_hit_at").
		Where("user_id = ? AND day >= ?", d.UserID, now.Add(-recentDays*24*time.Hour).Truncate(24*time.Hour)).Order("day").Find(&rows).Error
	d.Daily = make([]UserDay, len(rows))
	for i, r := range rows {
		d.Daily[i] = UserDay{Day: r.Day.Format(time.DateOnly), GroupID: r.GroupID, Audited: r.Audited, Suspicious: r.Suspicious,
			High: r.High, Errors: r.Errors, LastHitAt: utcPtr(r.LastHitAt)}
	}
	return err
}
