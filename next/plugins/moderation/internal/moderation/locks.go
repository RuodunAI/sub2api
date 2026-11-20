package moderation

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Locked entries (CONTRACTS §72.8; the source of truth since 0.11.0): the
// background-only list (allowlist) and the always-check-first list
// (blocklist) are the locked entries of the watch list, and the level of a
// locked entry is its list - observe = allowlist, enforce = blocklist. A
// locked entry never expires, has source manual (the CHECK of
// watchlist.source has no value for it) and is changed only through PUT and
// DELETE /watchlist/:user_id/lock; the other watch routes answer 409 locked
// (writeWatch).

// Lock kinds (Watch.Lock): the list of a locked entry.
const (
	LockAllowlist = "allowlist"
	LockBlocklist = "blocklist"
)

// lockList is a list: the level of its entries and the history reasons.
type lockList struct {
	level          string
	added, removed string
}

var lockLists = map[string]lockList{
	LockAllowlist: {LevelObserve,
		"加入只后台审核名单 / added to the background-only list",
		"移出只后台审核名单 / removed from the background-only list"},
	LockBlocklist: {LevelEnforce,
		"加入总是先审名单 / added to the always-check-first list",
		"移出总是先审名单 / removed from the always-check-first list"},
}

// lockOf names the list of an entry at level ("" = not locked).
func lockOf(locked bool, level string) string {
	switch {
	case !locked:
		return ""
	case level == LevelEnforce:
		return LockBlocklist
	}
	return LockAllowlist
}

// lockEntry makes w, uid's live entry (found = there is one), a locked
// entry of list, recorded as a lock with c's reason and operator. A new
// level opens a new counting window, as a level change by hand does
// (updateWatch). An entry that is one already is left alone (changed =
// false). The caller holds the user's lock and bumps watch_version.
func lockEntry(tx *gorm.DB, w Watch, found bool, list string, now time.Time, c watchChange) (Watch, bool, error) {
	if found && w.Lock == list && w.ExpiresAt == nil {
		return w, false, nil
	}
	c.action, c.from, c.to = "lock", "", lockLists[list].level
	if found {
		c.from = w.Level
	}
	// A new level opens a new counting window; the same level keeps it.
	same := func(col string) clause.Expr {
		return gorm.Expr("CASE WHEN "+model.Prefix+"watchlist.level = EXCLUDED.level THEN ? ELSE ? END",
			clause.Column{Table: model.Prefix + "watchlist", Name: col}, clause.Column{Table: "excluded", Name: col})
	}
	row := model.Watch{UserID: c.uid, Level: c.to, Source: "manual", SinceAt: now, ViolationEpoch: newEpoch(), Locked: true, UpdatedAt: now}
	err := tx.Clauses(clause.OnConflict{
		Columns: columns("user_id"),
		DoUpdates: append(clause.AssignmentColumns([]string{"level", "updated_at"}), clause.Assignments(map[string]any{
			"source": "manual", "expires_at": nil, "locked": true,
			"since_at": same("since_at"), "violation_epoch": same("violation_epoch"),
			"violation_count": gorm.Expr("CASE WHEN " + model.Prefix + "watchlist.level = EXCLUDED.level THEN " + model.Prefix + "watchlist.violation_count ELSE 0 END"),
		})...),
	}, clause.Returning{}).Create(&row).Error
	w = watchOf(row)
	if err != nil {
		return w, false, err
	}
	return w, true, watchEvent(tx, c)
}

// lockInput is the PUT /watchlist/:user_id/lock body.
type lockInput struct {
	List string `json:"list"`
}

// lockWatch puts a user on a list: the user's entry, or a new one, becomes
// a locked entry of the list. Moderators cannot be locked, as they cannot
// be watched.
func (p *Plugin) lockWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	var in lockInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	errs := p.staffUser(ctx, uid)
	if _, ok := lockLists[in.List]; !ok {
		errs = errs.Add("list", "enum", "list must be allowlist or blocklist / list 取值为 allowlist、blocklist")
	}
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid lock / 锁定参数无效", errs), nil
	}
	if resp := p.missingUser(ctx, uid); resp != nil {
		return resp, nil
	}
	return p.changeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, now time.Time) (Watch, error) {
		w, changed, err := lockEntry(tx, w, found, in.List, now, watchChange{uid: uid, reason: lockLists[in.List].added, operator: callerID(req)})
		if err == nil && !changed {
			err = errUnchanged
		}
		return w, err
	})
}

// unlockWatch takes a user off their list: the locked entry is removed.
func (p *Plugin) unlockWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	return p.changeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, _ time.Time) (Watch, error) {
		if !found || !w.Locked {
			return w, errNotLocked
		}
		return w, unlockEntry(tx, w, lockLists[w.Lock].removed, callerID(req))
	})
}

// unlockEntry removes w, a locked entry, recorded as an unlock with reason
// by operator (nil = the list import). The caller holds the user's lock.
func unlockEntry(tx *gorm.DB, w Watch, reason string, operator *int64) error {
	return removeEntry(tx, watchChange{uid: w.UserID, action: "unlock", from: w.Level, reason: reason, operator: operator})
}
