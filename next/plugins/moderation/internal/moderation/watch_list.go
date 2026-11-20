package moderation

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// GET /watchlist like new-api ListModerationWatch: filters level, new_only,
// locked, category, user_id / user_ids (the console resolves usernames to ids
// through the core), sort new | last_hit | severity | total, and per-row
// aggregates of the user's suspicious records.

// recentDays is the window of a watch row's groups (new-api moderationRecentDays).
const recentDays = 30

// suspiciousResult selects the suspicious records; severityCounts selects
// total, high, medium and low of them (with suspiciousResult in the WHERE
// clause).
const (
	suspiciousResult = `result IN ('hit', 'block')`
	severityCounts   = `count(*) AS total, count(*) FILTER (WHERE severity = 'high') AS high,
	count(*) FILTER (WHERE severity = 'medium') AS medium, count(*) FILTER (WHERE severity = 'low') AS low`
)

// SeverityCounts counts suspicious records by severity.
type SeverityCounts struct {
	High   int64 `json:"high"`
	Medium int64 `json:"medium"`
	Low    int64 `json:"low"`
}

// WatchItem is one row of GET /watchlist.
type WatchItem struct {
	Watch
	// Allowlisted, Blocklisted: the entry puts the user on that list
	// (Watch.Lock).
	Allowlisted     bool            `json:"allowlisted"`
	Blocklisted     bool            `json:"blocklisted"`
	Staff           bool            `json:"staff"`    // a moderator (isStaff), not to be locked: the entry predates that
	GroupID         int64           `json:"group_id"` // group of the latest record
	OtherGroups     []int64         `json:"other_groups"`
	Flow            *FlowRef        `json:"flow"` // flow bound to group_id now
	SuspiciousTotal int64           `json:"suspicious_total"`
	BySeverity      SeverityCounts  `json:"by_severity"`
	TopCategories   []CategoryCount `json:"top_categories"`
	TopOther        int64           `json:"top_other"`
}

// watchFilter builds the conditions over watchlist w.
func watchFilter(req *pluginv1.HTTPRequest, now time.Time) *sqlFilter {
	f := &sqlFilter{}
	f.add("(w.expires_at IS NULL OR w.expires_at > ?)", now)
	if v := pluginsdk.Query(req, "level"); v != "" {
		if v != LevelObserve && v != LevelEnforce {
			f.errs = f.errs.Add("level", "enum", "level must be observe or enforce / level 取值为 observe、enforce")
		} else {
			f.eq("w.level", v)
		}
	}
	switch pluginsdk.Query(req, "new_only") {
	case "", "0", "false":
	case "1", "true":
		f.add("w.hits_since_seen > 0")
	default:
		f.errs = f.errs.Add("new_only", "enum", "new_only must be true or false / new_only 取值为 true、false")
	}
	f.flag(req, "locked", "w.locked")
	if v := pluginsdk.Query(req, "category"); v != "" {
		if !jevCategoryRe.MatchString(v) && !categoryIDRe.MatchString(v) {
			f.errs = f.errs.Add("category", "invalid", "category is invalid / 分类格式无效")
		} else {
			// By the record's highest category, like the row's top categories.
			f.add("EXISTS (SELECT 1 FROM "+model.Prefix+"events e WHERE e.user_id = w.user_id AND e."+suspiciousResult+" AND e.highest_category = ?)", v)
		}
	}
	f.id(req, "user_id", "w.user_id")
	f.idList(req, "user_ids", "w.user_id")
	return f
}

func (p *Plugin) listWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	page, size, errs := pagination(req)
	now := p.now().UTC()
	f := watchFilter(req, now)
	errs = append(errs, f.errs...)
	var order pgdb.SQL = "w.hits_since_seen DESC, w.last_hit_at DESC NULLS LAST, w.user_id"
	severity := false
	switch sort := pluginsdk.Query(req, "sort"); sort {
	case "", "new":
	case "last_hit":
		order = "w.last_hit_at DESC NULLS LAST, w.user_id"
	case "severity":
		order, severity = "coalesce(r.high, 0) DESC, coalesce(r.medium, 0) DESC, w.user_id", true
	case "total":
		order, severity = "coalesce(r.total, 0) DESC, w.user_id", true
	default:
		errs = errs.Add("sort", "enum", "sort must be new, last_hit, severity or total / sort 取值为 new、last_hit、severity、total")
	}
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid query / 查询参数无效", errs), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	watched := func() *gorm.DB { return db.Table(model.Prefix + "watchlist w").Scopes(f.scope) }
	var total int64
	if err := watched().Count(&total).Error; err != nil {
		return nil, err
	}
	q := watched()
	if severity {
		// The suspicious records of the listed users, counted by severity.
		q = q.Joins("LEFT JOIN (?) r ON r.user_id = w.user_id", db.Model(&model.Event{}).Select("user_id, "+severityCounts).
			Where(suspiciousResult).Where("user_id IN (?)", watched().Select("w.user_id")).Group("user_id"))
	}
	var rows []model.Watch
	if err := q.Select("w.*").Order(string(order)).Limit(size).Offset((page - 1) * size).Find(&rows).Error; err != nil {
		return nil, err
	}
	list := make([]Watch, len(rows))
	for i, m := range rows {
		list[i] = watchOf(m)
	}
	items, err := p.watchItems(ctx, db, list, now)
	if err != nil {
		return nil, err
	}
	counts, err := p.watchCounts(db)
	if err != nil {
		return nil, err
	}
	return pluginsdk.JSONResponse(http.StatusOK, map[string]any{
		"data": items, "page": pluginsdk.Page{Page: page, PageSize: size, Total: total},
		"counts": counts,
	}), nil
}

// watchRows is what watchItems reads about the listed entries before it
// renders them.
type watchRows struct {
	ids  []int64
	byID map[int64]*WatchItem
	// categories counts each user's suspicious records by highest category.
	categories map[int64]map[string]int64
	// grouped: the user has records of the last recentDays (GroupID is set).
	grouped map[int64]bool
}

// watchItems adds the aggregates of each entry's suspicious records, its
// recent groups and the flow of its current group.
func (p *Plugin) watchItems(ctx context.Context, db *gorm.DB, list []Watch, now time.Time) ([]WatchItem, error) {
	items := make([]WatchItem, len(list))
	if len(list) == 0 {
		return items, nil
	}
	r := &watchRows{byID: map[int64]*WatchItem{}, categories: map[int64]map[string]int64{}, grouped: map[int64]bool{}}
	for i, w := range list {
		items[i] = WatchItem{Watch: w, Allowlisted: w.Lock == LockAllowlist, Blocklisted: w.Lock == LockBlocklist,
			Staff: p.isStaff(ctx, w.UserID), OtherGroups: []int64{}, TopCategories: []CategoryCount{}}
		r.ids = append(r.ids, w.UserID)
		r.byID[w.UserID] = &items[i]
	}
	for _, load := range []func(context.Context, *gorm.DB, *watchRows, time.Time) error{
		loadWatchRecords, loadWatchGroups,
	} {
		if err := load(ctx, db, r, now); err != nil {
			return nil, err
		}
	}
	snap := p.snap.Load()
	labels := snap.categoryLabels()
	for i := range items {
		it := &items[i]
		it.TopCategories, it.TopOther = topCategories(r.categories[it.UserID], it.SuspiciousTotal, labels)
		if f, _ := snap.flowFor(it.GroupID); r.grouped[it.UserID] && f != nil {
			it.Flow = &FlowRef{FlowID: f.ID, Name: f.Name, Version: f.Version}
		}
	}
	return items, nil
}

// topCategories ranks the highest categories of a user's suspicious records:
// the top 3, and the records outside them (new-api ListModerationWatch:
// records without a category count there too).
func topCategories(counts map[string]int64, total int64, labels map[string]string) ([]CategoryCount, int64) {
	top := make([]CategoryCount, 0, len(counts))
	for c, n := range counts {
		top = append(top, CategoryCount{Category: c, Label: labels[c], Count: n})
	}
	slices.SortFunc(top, func(a, b CategoryCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Category, b.Category))
	})
	top = top[:min(3, len(top))]
	other := total
	for _, c := range top {
		other -= c.Count
	}
	return top, other
}

// loadWatchRecords counts the suspicious records by severity and highest
// category.
func loadWatchRecords(ctx context.Context, db *gorm.DB, r *watchRows, _ time.Time) error {
	var rows []struct {
		UserID                    int64
		HighestCategory, Severity string
		N                         int64
	}
	err := db.Model(&model.Event{}).Select("user_id, highest_category, severity, count(*) AS n").
		Where(suspiciousResult+" AND user_id = ANY(?)", pgdb.Array[int64](r.ids)).Group("1, 2, 3").Scan(&rows).Error
	for _, row := range rows {
		uid, n := row.UserID, row.N
		it := r.byID[uid]
		it.SuspiciousTotal += n
		switch row.Severity {
		case "high":
			it.BySeverity.High += n
		case "medium":
			it.BySeverity.Medium += n
		case "low":
			it.BySeverity.Low += n
		}
		if cat := row.HighestCategory; cat != "" {
			if r.categories[uid] == nil {
				r.categories[uid] = map[string]int64{}
			}
			r.categories[uid][cat] += n
		}
	}
	return err
}

// loadWatchGroups reads the groups of the last recentDays, newest first: the
// first is the current one.
func loadWatchGroups(ctx context.Context, db *gorm.DB, r *watchRows, now time.Time) error {
	var rows []model.Event
	err := db.Select("user_id, group_id").Where("user_id = ANY(?) AND created_at >= ?", pgdb.Array[int64](r.ids), now.Add(-recentDays*24*time.Hour)).
		Group("user_id, group_id").Order("user_id, max(created_at) DESC").Find(&rows).Error
	for _, row := range rows {
		it := r.byID[row.UserID]
		if !r.grouped[row.UserID] {
			r.grouped[row.UserID], it.GroupID = true, row.GroupID
		} else {
			it.OtherGroups = append(it.OtherGroups, row.GroupID)
		}
	}
	return err
}

// markWatchSeen resets hits_since_seen of an active entry (new-api "mark as seen").
func (p *Plugin) markWatchSeen(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	res := db.Model(&model.Watch{}).Where("user_id = ? AND (expires_at IS NULL OR expires_at > ?)", uid, p.now().UTC()).Update("hits_since_seen", 0)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return pluginsdk.ErrorResponse(http.StatusNotFound, "not_found", "user is not on the watch list / 该用户不在观察名单中"), nil
	}
	return pluginsdk.DataResponse(map[string]any{"user_id": uid, "hits_since_seen": 0}), nil
}
