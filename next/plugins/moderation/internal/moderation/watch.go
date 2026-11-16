package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Watch is one watch list entry (CONTRACTS §72.8).
type Watch struct {
	UserID         int64      `json:"user_id"`
	Level          string     `json:"level"`
	Source         string     `json:"source"`
	SinceAt        time.Time  `json:"since_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	HitCount       int        `json:"hit_count"`
	ViolationCount int        `json:"violation_count"`
	ViolationEpoch int64      `json:"-"`
	LastHitAt      *time.Time `json:"last_hit_at"`
	// HitsSinceSeen counts suspicious records since an administrator last
	// marked the entry seen (new-api hits_since_mark).
	HitsSinceSeen int       `json:"hits_since_seen"`
	Note          string    `json:"note"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Locked: the entry puts the user on a list (locks.go) and is changed
	// through the lock routes only; Lock names the list (allowlist |
	// blocklist), which is the entry's level.
	Locked bool   `json:"locked"`
	Lock   string `json:"lock,omitempty"`
}

// watchOf is the entry of a stored row, its times in UTC.
func watchOf(m model.Watch) Watch {
	return Watch{UserID: m.UserID, Level: m.Level, Source: m.Source, SinceAt: m.SinceAt.UTC(), ExpiresAt: utcPtr(m.ExpiresAt),
		HitCount: m.HitCount, ViolationCount: m.ViolationCount, ViolationEpoch: m.ViolationEpoch, LastHitAt: utcPtr(m.LastHitAt),
		HitsSinceSeen: m.HitsSinceSeen, Note: m.Note, UpdatedAt: m.UpdatedAt.UTC(), Locked: m.Locked, Lock: lockOf(m.Locked, m.Level)}
}

// takeWatch reads the entry q selects (gorm.ErrRecordNotFound: none).
func takeWatch(q *gorm.DB) (Watch, error) {
	var m model.Watch
	err := q.Take(&m).Error
	return watchOf(m), err
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// loadWatch reads (and locks) one entry; the caller holds the user's
// block_subjects lock.
func loadWatch(tx *gorm.DB, uid int64) (Watch, bool, error) {
	w, err := takeWatch(tx.Clauses(forUpdate()).Where("user_id = ?", uid))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Watch{}, false, nil
	}
	return w, err == nil, err
}

// watchChange is one row of the watch history: action from one level to
// another, by operator (nil = automatic).
type watchChange struct {
	uid              int64
	action, from, to string
	reason           string
	operator         *int64
}

func watchEvent(tx *gorm.DB, c watchChange) error {
	return tx.Create(&model.WatchEvent{UserID: c.uid, Action: c.action, FromLevel: c.from, ToLevel: c.to,
		Reason: truncateRunes(c.reason, 500), OperatorID: c.operator}).Error
}

// removeEntry deletes an entry and records c (c.from is the level it had).
func removeEntry(tx *gorm.DB, c watchChange) error {
	if err := tx.Where("user_id = ?", c.uid).Delete(&model.Watch{}).Error; err != nil {
		return err
	}
	if err := watchEvent(tx, c); err != nil {
		return err
	}
	return bumpWatch(tx)
}

// expireEntry removes an entry whose expiry has passed.
func expireEntry(tx *gorm.DB, w Watch) error {
	return removeEntry(tx, watchChange{uid: w.UserID, action: "expire", from: w.Level, reason: "expired"})
}

// WatchEvent is one row of the watch history.
type WatchEvent struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Action     string    `json:"action"`
	FromLevel  string    `json:"from_level"`
	ToLevel    string    `json:"to_level"`
	Reason     string    `json:"reason"`
	OperatorID *int64    `json:"operator_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func callerID(req *pluginv1.HTTPRequest) *int64 {
	if id := req.GetCaller().GetUserId(); id > 0 {
		return &id
	}
	return nil
}

// watchInput is the POST /watchlist and PATCH /watchlist/:user_id body.
// days 0 = no expiry; absent = keep (PATCH) or watch_days (POST);
// expires_at sets an absolute expiry instead ("" = none). reason goes to
// the history.
type watchInput struct {
	UserID    json.Number `json:"user_id"`
	Level     *string     `json:"level"`
	Days      *int        `json:"days"`
	ExpiresAt *string     `json:"expires_at"`
	Note      *string     `json:"note"`
	Reason    *string     `json:"reason"`
}

// watchValues are the parsed values of a watchInput.
type watchValues struct {
	uid        int64 // create only
	days       *int
	expires    *time.Time // expires_at (nil = none)
	hasExpires bool       // expires_at was sent
}

func (in watchInput) validate(create bool) (v watchValues, errs pluginsdk.FieldErrors) {
	v.days = in.Days
	if create {
		uid, err := strconv.ParseInt(in.UserID.String(), 10, 64)
		v.uid = uid
		if err != nil || uid <= 0 {
			errs = errs.Add("user_id", "invalid", "user_id must be a positive integer / user_id 必须是正整数")
		}
	}
	if in.Level != nil && *in.Level != LevelObserve && *in.Level != LevelEnforce {
		errs = errs.Add("level", "enum", "level must be observe or enforce / level 取值为 observe、enforce")
	}
	if in.Days != nil && (*in.Days < 0 || *in.Days > 3650) {
		errs = errs.Add("days", "out_of_range", "days must be between 0 and 3650 / days 取值 0–3650")
	}
	if in.Note != nil && utf8.RuneCountInString(*in.Note) > 500 {
		errs = errs.Add("note", "too_long", "note must be at most 500 characters / 备注最多 500 字")
	}
	if in.Reason != nil && utf8.RuneCountInString(*in.Reason) > 500 {
		errs = errs.Add("reason", "too_long", "reason must be at most 500 characters / 原因最多 500 字")
	}
	if in.ExpiresAt != nil {
		v.hasExpires = true
		if s := strings.TrimSpace(*in.ExpiresAt); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				errs = errs.Add("expires_at", "format", "expires_at must be RFC 3339 / expires_at 必须是 RFC 3339 时间")
			} else {
				t = t.UTC()
				v.expires = &t
			}
		}
	}
	return v, errs
}

// expiry applies days or expires_at; ok = one of them was given.
func (v watchValues) expiry(c *config, now time.Time) (*time.Time, bool) {
	if v.hasExpires {
		return v.expires, true
	}
	if v.days != nil {
		return expiryOf(v.days, c, now), true
	}
	return nil, false
}

// historyReason is the reason written to the history (reason, else note).
func (in watchInput) historyReason(note string) string {
	if in.Reason != nil {
		return strings.TrimSpace(*in.Reason)
	}
	return note
}

// staffUser refuses to put a moderator (staff) on the watch list or under a
// ban by hand: the hook never watches or bans them either (new-api:
// administrators).
func (p *Plugin) staffUser(ctx context.Context, uid int64) pluginsdk.FieldErrors {
	if p.isStaff(ctx, uid) {
		return pluginsdk.FieldErrors{}.Add("user_id", "staff", "moderators are not watched or banned / 审核管理员不进入观察名单，也不会被封禁")
	}
	return nil
}

// unbannableUser refuses a ban by hand: moderators, and allowlisted users,
// who are never refused (a ban is heavier than a refusal).
func (p *Plugin) unbannableUser(ctx context.Context, uid int64) pluginsdk.FieldErrors {
	if p.userLock(uid) == LockAllowlist {
		return pluginsdk.FieldErrors{}.Add("user_id", "allowlisted", "allowlisted users cannot be banned / 白名单用户不能封禁")
	}
	return p.staffUser(ctx, uid)
}

func decodeBody(req *pluginv1.HTTPRequest, v any) *pluginv1.HTTPResponse {
	dec := json.NewDecoder(strings.NewReader(string(req.GetBody())))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", "invalid JSON body / 请求体不是合法 JSON")
	}
	return nil
}

func (p *Plugin) createWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in watchInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	c := p.cfg.Load()
	v, errs := in.validate(true)
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid watch entry / 名单参数无效", errs), nil
	}
	uid := v.uid
	if errs := p.staffUser(ctx, uid); len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid watch entry / 名单参数无效", errs), nil
	}
	if resp := p.missingUser(ctx, uid); resp != nil {
		return resp, nil
	}
	level := LevelObserve
	if in.Level != nil {
		level = *in.Level
	}
	return p.writeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, now time.Time) (Watch, error) {
		if found {
			return w, errConflict
		}
		w = Watch{UserID: uid, Level: level, Source: "manual", SinceAt: now, UpdatedAt: now, ViolationEpoch: newEpoch()}
		// Without days or expires_at an observe entry expires after
		// watch_days and an enforce entry never (new-api addModerationWatch).
		var ok bool
		if w.ExpiresAt, ok = v.expiry(c, now); !ok && level == LevelObserve {
			w.ExpiresAt = expiryOf(nil, c, now)
		}
		if in.Note != nil {
			w.Note = strings.TrimSpace(*in.Note)
		}
		err := tx.Create(&model.Watch{UserID: uid, Level: w.Level, Source: w.Source, SinceAt: now, ExpiresAt: w.ExpiresAt,
			ViolationEpoch: w.ViolationEpoch, Note: w.Note, UpdatedAt: now}).Error
		if err != nil {
			return w, err
		}
		return w, watchEvent(tx, watchChange{uid: uid, action: "add", to: level, reason: in.historyReason(w.Note),
			operator: callerID(req)})
	})
}

// expiryOf: days nil = watch_days, 0 = none.
func expiryOf(days *int, c *config, now time.Time) *time.Time {
	d := c.WatchDays
	if days != nil {
		d = *days
	}
	if d == 0 {
		return nil
	}
	t := now.Add(time.Duration(d) * 24 * time.Hour)
	return &t
}

func (p *Plugin) updateWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	var in watchInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	c := p.cfg.Load()
	v, errs := in.validate(false)
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid watch entry / 名单参数无效", errs), nil
	}
	return p.writeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, now time.Time) (Watch, error) {
		if !found {
			return w, errNotFound
		}
		from := w.Level
		if in.Level != nil {
			w.Level = *in.Level
		}
		changed := from != w.Level
		if changed {
			// A new level opens a new counting window (new-api
			// updateModerationWatch): violations, epoch, since and the new
			// hits restart; back at observe the entry expires after
			// watch_days again, at enforce never, unless days or expires_at
			// says otherwise.
			w.ViolationEpoch, w.ViolationCount, w.SinceAt = newEpoch(), 0, now
			w.Source, w.HitsSinceSeen, w.ExpiresAt = "manual", 0, nil
			if w.Level == LevelObserve {
				w.ExpiresAt = expiryOf(nil, c, now)
			}
		}
		if exp, ok := v.expiry(c, now); ok {
			w.ExpiresAt = exp
		}
		if in.Note != nil {
			w.Note = strings.TrimSpace(*in.Note)
		}
		w.UpdatedAt = now
		set := map[string]any{"level": w.Level, "expires_at": w.ExpiresAt, "note": w.Note, "updated_at": now}
		if changed {
			set["source"], set["since_at"], set["violation_count"] = w.Source, now, 0
			set["violation_epoch"], set["hits_since_seen"] = w.ViolationEpoch, 0
		}
		if err := tx.Model(&model.Watch{}).Where("user_id = ?", uid).Updates(set).Error; err != nil {
			return w, err
		}
		action := "update"
		if changed {
			action = "set_level"
		} else if in.Note != nil && in.Level == nil && in.Days == nil && !v.hasExpires {
			action = "note"
		}
		return w, watchEvent(tx, watchChange{uid: uid, action: action, from: from, to: w.Level, reason: in.historyReason(w.Note),
			operator: callerID(req)})
	})
}

// deleteWatch removes an entry; the optional query reason goes to the
// history.
func (p *Plugin) deleteWatch(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	reason := strings.TrimSpace(pluginsdk.Query(req, "reason"))
	return p.writeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, _ time.Time) (Watch, error) {
		if !found {
			return w, errNotFound
		}
		return w, removeEntry(tx, watchChange{uid: uid, action: "remove", from: w.Level, reason: reason,
			operator: callerID(req)})
	})
}

var (
	errConflict  = errors.New("conflict")
	errLocked    = errors.New("locked")
	errNotLocked = errors.New("not locked")
	// errUnchanged ends a change that wrote nothing: the entry is answered
	// without a bump or broadcast.
	errUnchanged = errors.New("unchanged")
)

// watchChangeFn changes uid's live entry w (found = there is one) at now.
type watchChangeFn func(tx *gorm.DB, w Watch, found bool, now time.Time) (Watch, error)

// writeWatch runs one manual change of an entry that is not locked (a
// locked entry is changed through the lock routes only, locks.go: the
// change is refused).
func (p *Plugin) writeWatch(ctx context.Context, req *pluginv1.HTTPRequest, uid int64, fn watchChangeFn) (*pluginv1.HTTPResponse, error) {
	return p.changeWatch(ctx, req, uid, func(tx *gorm.DB, w Watch, found bool, now time.Time) (Watch, error) {
		if found && w.Locked {
			return w, errLocked
		}
		return fn(tx, w, found, now)
	})
}

// changeWatch runs one manual watch change under the user's lock (an
// expired entry is removed first), then reloads and broadcasts; a change
// that wrote nothing (errUnchanged) answers the entry as it is.
func (p *Plugin) changeWatch(ctx context.Context, req *pluginv1.HTTPRequest, uid int64, fn watchChangeFn) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	now := p.now().UTC()
	var out Watch
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockBlockSubject(tx, uid); err != nil {
			return err
		}
		w, found, _, err := liveWatchEntry(tx, uid, now)
		if err != nil {
			return err
		}
		if out, err = fn(tx, w, found, now); err != nil {
			return err
		}
		return bumpWatch(tx)
	})
	switch {
	case errors.Is(err, errUnchanged):
		return pluginsdk.DataResponse(out), nil
	case errors.Is(err, errNotFound):
		return pluginsdk.ErrorResponse(http.StatusNotFound, "not_found", "user is not on the watch list / 该用户不在观察名单中"), nil
	case errors.Is(err, errConflict):
		return pluginsdk.ErrorResponse(http.StatusConflict, "already_watched", "user is already on the watch list / 该用户已在观察名单中"), nil
	case errors.Is(err, errNotLocked):
		return pluginsdk.ErrorResponse(http.StatusNotFound, "not_locked", "user is on neither list / 该用户不在“只后台审核”或“总是先审”名单中"), nil
	case errors.Is(err, errLocked):
		return pluginsdk.ErrorResponse(http.StatusConflict, "locked",
			"the entry puts the user on the background-only / always-check-first list: unlock it first / 该条目属于“只后台审核”或“总是先审”名单，请先解锁"), nil
	case err != nil:
		return nil, err
	}
	p.watchChanged(ctx)
	if req.GetMethod() == http.MethodDelete {
		return pluginsdk.DataResponse(map[string]any{"user_id": uid, "removed": true}), nil
	}
	return pluginsdk.DataResponse(out), nil
}

func (p *Plugin) listWatchEvents(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	uid, bad := parseID(req, "user_id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	var rows []model.WatchEvent
	if err := db.Where("user_id = ?", uid).Order("id DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	list := make([]WatchEvent, len(rows))
	for i, e := range rows {
		list[i] = WatchEvent{ID: e.ID, UserID: e.UserID, Action: e.Action, FromLevel: e.FromLevel, ToLevel: e.ToLevel,
			Reason: e.Reason, OperatorID: e.OperatorID, CreatedAt: e.CreatedAt.UTC()}
	}
	return pluginsdk.DataResponse(list), nil
}
