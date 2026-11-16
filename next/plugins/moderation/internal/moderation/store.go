package moderation

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
)

// eventRec is one moderation record (table events), written in batches.
type eventRec struct {
	At               time.Time
	RequestID        string
	UserID           int64
	APIKeyID         int64
	GroupID          int64
	Model            string
	Protocol         string
	Endpoint         string // RequestMeta.endpoint (path pattern)
	SessionFp        string // RequestMeta.session_fingerprint ("" = no sticky session)
	Mode             string
	Timing           string
	FlowID           int64
	FlowVersion      int
	Result           string
	Severity         string
	HitCategories    []string
	HighestCategory  string
	HighestScore     float64
	Trace            []byte
	InputFormat      string
	Text             *string
	TextChars        int
	TextHash         string
	ImageCount       int
	InputMessage     *string
	InputMessageSize int
	LatencyMs        int
	QueueMs          int
	KeepUntil        time.Time
	// Replayed: a cached hit replayed (hook.go replay), not a new violation.
	Replayed bool
	// FlowSource and FlowFallback: how the request found its flow
	// (flowMatch, CONTRACTS §72.17, 0.10.0); NULL when "".
	FlowSource   string
	FlowFallback *StaleBinding
	// Level, LevelSource: the handling that applied (decideHandling); ""
	// is stored as NULL.
	Level       string
	LevelSource string
}

// fillFromTrace derives result, categories and severity.
func (r *eventRec) fillFromTrace(tr Trace, refused bool) {
	switch v := tr.verdict(); {
	case v == ResultHit && refused:
		r.Result = ResultBlock
	default:
		r.Result = v
	}
	r.HitCategories = []string{}
	for _, h := range tr.HitCategories() {
		r.HitCategories = append(r.HitCategories, h.Category)
		if r.HighestCategory == "" {
			r.HighestCategory, r.HighestScore = h.Category, h.Score
		}
	}
	if tr.Final == Hit {
		r.Severity = severityOf(r.HighestScore)
	}
	r.Trace, _ = json.Marshal(tr)
}

// legacyColumns fills events.verdict / action for 0.1 readers.
func (r *eventRec) legacyColumns() (verdict, action string) {
	switch r.Result {
	case ResultBlock:
		return VerdictBlock, ActionDeny
	case ResultHit:
		return VerdictFlag, ActionAllow
	case ResultError:
		return VerdictError, ActionAllow
	}
	return VerdictPass, ActionAllow
}

// fallbackColumns are events.flow_fallback_id and flow_fallback_reason (NULL
// without a fallback).
func (r *eventRec) fallbackColumns() (*int64, *string) {
	if r.FlowFallback == nil {
		return nil, nil
	}
	return &r.FlowFallback.FlowID, &r.FlowFallback.Reason
}

// dropInputMessages removes the input messages of a batch that failed to
// write and notes the reason in their traces; false = none had one.
func dropInputMessages(evs []eventRec, reason string) bool {
	dropped := false
	for i := range evs {
		e := &evs[i]
		if e.InputMessage == nil {
			continue
		}
		var tr Trace
		if json.Unmarshal(e.Trace, &tr) == nil {
			tr.InputMessageDropped = reason
			e.Trace, _ = json.Marshal(tr)
		}
		e.InputMessage, e.InputMessageSize, dropped = nil, 0, true
	}
	return dropped
}

func (r *eventRec) suspicious() bool { return r.Result == ResultHit || r.Result == ResultBlock }

// storable makes every text value of the record acceptable to PostgreSQL:
// text columns reject NUL and invalid UTF-8, jsonb also the \u0000 escape.
// The moderated text is normalized already (normalizeText); the metadata
// and the upstream answers in the trace are not under our control.
func (r *eventRec) storable() {
	for _, s := range []*string{&r.RequestID, &r.Model, &r.Protocol, &r.Endpoint, &r.SessionFp, &r.Mode, &r.Timing,
		&r.Result, &r.Severity, &r.HighestCategory, &r.InputFormat, &r.TextHash} {
		*s = pgText(*s)
	}
	for i := range r.HitCategories {
		r.HitCategories[i] = pgText(r.HitCategories[i])
	}
	if r.Text != nil {
		t := pgText(*r.Text)
		r.Text = &t
	}
	if r.InputMessage != nil {
		m := string(pgJSON([]byte(*r.InputMessage)))
		r.InputMessage, r.InputMessageSize = &m, len(m)
	}
	r.Trace = pgJSON(r.Trace)
}

// storeEvents writes records and applies them to the watch list and the
// bans. When the write fails the records are written again without their
// input messages: those are the largest and least checked part of a record
// (new-api worker.go).
func (p *Plugin) storeEvents(ctx context.Context, evs []eventRec) error {
	ids, err := p.writeEvents(ctx, evs)
	if err != nil && ctx.Err() == nil && dropInputMessages(evs, "save_failed") {
		ids, err = p.writeEvents(ctx, evs)
	}
	if err != nil {
		return err
	}
	p.afterWrite(ctx, evs, ids)
	return nil
}

// retrySuspicious writes the suspicious records of a failed batch once more,
// one by one so a single bad record cannot take the others with it: they
// drive the watch list, the violation counts and the bans. It returns how
// many were stored.
func (p *Plugin) retrySuspicious(ctx context.Context, evs []eventRec) int {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	kept := 0
	for _, e := range evs {
		if !e.suspicious() {
			continue
		}
		if err := p.storeEvents(rctx, []eventRec{e}); err != nil {
			p.log.Warn("moderation: suspicious record lost", "user_id", e.UserID, "request_id", e.RequestID, "error", err.Error())
			continue
		}
		kept++
	}
	return kept
}

// row is the events row of the record. The 0.1 columns it leaves zero
// (reason, error, cached, llm_model, turns, tokens) are what 0.2 on writes.
func (r *eventRec) row() model.Event {
	verdict, action := r.legacyColumns()
	fallbackID, fallbackReason := r.fallbackColumns()
	mode := r.Mode
	if mode != ModeEnforce {
		mode = ModeObserve
	}
	keepUntil := r.KeepUntil
	return model.Event{CreatedAt: r.At, RequestID: r.RequestID, UserID: r.UserID, APIKeyID: r.APIKeyID, GroupID: r.GroupID,
		Model: r.Model, Protocol: r.Protocol, Mode: mode, Verdict: verdict, Action: action, Categories: r.HitCategories,
		Severity: r.Severity, Text: r.Text, TextChars: r.TextChars, TextHash: r.TextHash, LatencyMs: r.LatencyMs, Result: r.Result,
		Timing: r.Timing, FlowID: r.FlowID, FlowVersion: r.FlowVersion, HighestCategory: r.HighestCategory,
		HighestScore: r.HighestScore, HitCategories: r.HitCategories, Trace: r.Trace, InputFormat: r.InputFormat,
		InputMessage: r.InputMessage, InputMessageSize: r.InputMessageSize, ImageCount: r.ImageCount, KeepUntil: &keepUntil,
		QueueMs: r.QueueMs, Endpoint: r.Endpoint, SessionFp: r.SessionFp, Replayed: r.Replayed,
		FlowSource: nullIfEmpty(r.FlowSource), FlowFallbackID: fallbackID, FlowFallbackReason: fallbackReason,
		Level: nullIfEmpty(r.Level), LevelSource: nullIfEmpty(r.LevelSource)}
}

// nullIfEmpty is NULL for "".
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// returningID makes an INSERT of events read back the ids only, not every
// column with a database default (GORM always reads something back).
var returningID = clause.Returning{Columns: columns("id")}

// writeEvents persists a batch, all of it or nothing, and returns the ids of
// its records. The ids are drawn first and written with the rows: the ids a
// multi-row INSERT returns come in an order PostgreSQL does not promise to
// be the order of its rows, so they could not be told apart.
func (p *Plugin) writeEvents(ctx context.Context, evs []eventRec) ([]int64, error) {
	db, err := p.db(ctx)
	if err != nil {
		return nil, err
	}
	// Drawn ascending: a batch's records get them in order, as they did
	// one INSERT each.
	ids, err := pgdb.NextIDs(db, "events", len(evs))
	if err != nil {
		return nil, err
	}
	rows := make([]model.Event, len(evs))
	for i := range evs {
		evs[i].storable()
		rows[i] = evs[i].row()
		rows[i].ID = ids[i]
	}
	if err := pgdb.InsertAll(db, rows, returningID); err != nil {
		return nil, err
	}
	return ids, nil
}

// afterWrite applies the watch list (§72.8) and the automatic ban to the
// suspicious records of a written batch. A replayed cached hit is not a new
// violation (history sent again would count once per turn): it neither
// watches the user nor counts a violation or towards a ban (new-api
// escalate.go onSuspicious).
func (p *Plugin) afterWrite(ctx context.Context, evs []eventRec, ids []int64) {
	c := p.cfg.Load()
	watchChanged, banned := false, map[int64]bool{}
	for i, e := range evs {
		if !e.suspicious() || e.Replayed || e.UserID <= 0 || p.isStaff(ctx, e.UserID) {
			continue
		}
		changed, err := p.recordWatchHit(ctx, c, e, ids[i])
		if err != nil {
			p.log.Warn("moderation: watch list update failed", "user_id", e.UserID, "error", err.Error())
		}
		watchChanged = watchChanged || changed
		// Allowlisted users are never banned (nor watched automatically,
		// enterWatch).
		if p.userLock(e.UserID) != LockAllowlist {
			banned[e.UserID] = true
		}
	}
	if watchChanged {
		p.watchChanged(ctx)
	}
	if len(banned) > 0 {
		p.applyBans(ctx, banned)
	}
}

func newEpoch() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
}

// recordWatchHit applies a violation to the watch list: escalation or
// auto_watch (enterWatch), then the count on the user's entry (one per
// request id; see countWatchHit for the levels that count). changed = the
// level set changed.
func (p *Plugin) recordWatchHit(ctx context.Context, c *config, e eventRec, eventID int64) (changed bool, err error) {
	db, err := p.db(ctx)
	if err != nil {
		return false, err
	}
	now := p.now().UTC()
	err = db.Transaction(func(tx *gorm.DB) error {
		changed = false
		if err := lockBlockSubject(tx, e.UserID); err != nil {
			return err
		}
		w, found, expired, err := liveWatchEntry(tx, e.UserID, now)
		if err != nil {
			return err
		}
		w, found, entered, err := enterWatch(tx, c, w, found, e.UserID, now)
		if err != nil {
			return err
		}
		changed = expired || entered
		if found {
			if err := countWatchHit(tx, c, w, e, eventID, now); err != nil {
				return err
			}
		}
		if changed {
			return bumpWatch(tx)
		}
		return nil
	})
	return changed, err
}

// liveWatchEntry returns the user's unexpired watch entry (found=false: the
// user is not watched); an expired one is removed first (expired).
func liveWatchEntry(tx *gorm.DB, uid int64, now time.Time) (w Watch, found, expired bool, err error) {
	if w, found, err = loadWatch(tx, uid); err != nil || !found {
		return w, false, false, err
	}
	if w.ExpiresAt != nil && !now.Before(*w.ExpiresAt) {
		return w, false, true, expireEntry(tx, w)
	}
	return w, true, false, nil
}

// enterWatch applies a violation's automatic watch changes in the order
// ban > escalation > auto_watch (CONTRACTS §72.8 0.10.0): the ban
// (applyBans, after this) wins, so escalates skips a user the same
// violations ban; escalation puts the user at the enforce level, else
// auto_watch adds a user who is not on the list at the default level, so
// that under default enforce the new entry does not lower the user to
// observe (new-api autoAddModerationWatch). One change, one history row. An
// allowlisted user is only moderated in the background and already has an
// entry (the locked one), so auto_watch never adds one. entered: an entry
// was added or raised.
func enterWatch(tx *gorm.DB, c *config, w Watch, found bool, uid int64, now time.Time) (Watch, bool, bool, error) {
	n, up, err := escalates(tx, c, w, found, uid, now)
	if err != nil {
		return w, found, false, err
	}
	switch {
	case up:
		w, err = escalateEntry(tx, c, w, found, uid, n, now)
		return w, true, true, err
	case found || !c.AutoWatch:
		return w, found, false, nil
	}
	exp := now.Add(c.watchFor)
	w = Watch{UserID: uid, Level: c.defaultLevel, Source: "auto", SinceAt: now, ExpiresAt: &exp, ViolationEpoch: newEpoch()}
	err = tx.Create(&model.Watch{UserID: w.UserID, Level: w.Level, Source: w.Source, SinceAt: now, ExpiresAt: &exp,
		ViolationEpoch: w.ViolationEpoch, UpdatedAt: now}).Error
	if err != nil {
		return w, false, false, err
	}
	err = watchEvent(tx, watchChange{uid: uid, action: "auto_add", to: w.Level, reason: "首次可疑记录 / first suspicious record"})
	return w, err == nil, err == nil, err
}

// escalates reports whether a violation puts uid at the enforce level:
// escalate_violations violations within escalate_window_seconds, counted
// like the ban threshold (countViolations) and only after the user's latest
// manual watch change (one through the watch API, or a lock / unlock from
// the lock routes or the one-time import of the lists), so that an
// administrator's downgrade, removal or list change is not undone by the
// violations it already judged. Never for N = 0, a locked entry (the users
// of the lists) or an entry at enforce already, nor when the same
// violations ban the user (the ban refuses every request).
// n is the count.
func escalates(tx *gorm.DB, c *config, w Watch, found bool, uid int64, now time.Time) (n int64, up bool, err error) {
	if c.EscalateViolations <= 0 || (found && (w.Locked || w.Level == LevelEnforce)) {
		return 0, false, nil
	}
	since := now.Add(-c.escalateWindow)
	var manual *time.Time
	err = tx.Model(&model.WatchEvent{}).Select("max(created_at)").
		Where("user_id = ? AND (operator_id IS NOT NULL OR action IN ('lock', 'unlock'))", uid).Row().Scan(&manual)
	if err != nil {
		return 0, false, err
	}
	if manual != nil && manual.After(since) {
		since = *manual
	}
	if n, err = countViolations(tx, uid, since); err != nil || n < int64(c.EscalateViolations) {
		return n, false, err
	}
	_, banned, err := banReached(tx, c, uid, now)
	return n, !banned, err
}

// escalateEntry puts the user at the enforce level for watch_days: a new
// entry, or the observe entry raised, which opens a new counting window as
// a level change by hand does (updateWatch). The source is auto (the CHECK
// of watchlist.source has no other value for it); the history action
// auto_escalate tells it apart.
func escalateEntry(tx *gorm.DB, c *config, w Watch, found bool, uid, n int64, now time.Time) (Watch, error) {
	from := ""
	if found {
		from = w.Level
	}
	exp := now.Add(c.watchFor)
	w.UserID, w.Level, w.Source, w.SinceAt, w.ExpiresAt, w.ViolationEpoch, w.ViolationCount = uid, LevelEnforce, "auto", now, &exp, newEpoch(), 0
	err := tx.Clauses(clause.OnConflict{
		Columns:   columns("user_id"),
		DoUpdates: clause.AssignmentColumns([]string{"level", "source", "since_at", "expires_at", "violation_epoch", "violation_count", "updated_at"}),
	}).Create(&model.Watch{UserID: w.UserID, Level: LevelEnforce, Source: "auto", SinceAt: now, ExpiresAt: &exp,
		ViolationEpoch: w.ViolationEpoch, UpdatedAt: now}).Error
	if err != nil {
		return w, err
	}
	return w, watchEvent(tx, watchChange{uid: w.UserID, action: "auto_escalate", from: from, to: LevelEnforce,
		reason: violationsReason(n, c.EscalateWindowSeconds)})
}

// violationsReason is the reason of an escalation or a ban after n
// violations within a window of sec seconds, e.g. "3 violations within 10m
// / 10 分钟内违规 3 次".
func violationsReason(n int64, sec int) string {
	en, zh := windowText(sec)
	return fmt.Sprintf("%d violations within %s / %s内违规 %d 次", n, en, zh, n)
}

// windowText formats a window of seconds in its largest whole unit (h, m
// or s), in English and Chinese.
func windowText(sec int) (en, zh string) {
	switch {
	case sec%3600 == 0:
		return fmt.Sprintf("%dh", sec/3600), fmt.Sprintf("%d 小时", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%dm", sec/60), fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%ds", sec), fmt.Sprintf("%d 秒", sec)
}

// countWatchHit counts a hit on the entry w. An observe entry, and under
// default enforce an enforce entry too, counts the violation once per
// request id; only an observe entry extends its expiry (new-api
// ApplyWatchHit).
func countWatchHit(tx *gorm.DB, c *config, w Watch, e eventRec, eventID int64, now time.Time) error {
	counts := w.Level == LevelObserve || (c.defaultLevel == LevelEnforce && w.Level == LevelEnforce)
	if !counts {
		return tx.Model(&model.Watch{}).Where("user_id = ?", e.UserID).Updates(map[string]any{"hit_count": gorm.Expr("hit_count + 1"),
			"hits_since_seen": gorm.Expr("hits_since_seen + 1"), "last_hit_at": now, "updated_at": now}).Error
	}
	counted := false
	if e.RequestID != "" {
		var err error
		counted, err = pgdb.Exists(tx, tx.Model(&model.Event{}).
			Where("user_id = ? AND request_id = ? AND counted AND id <> ?", e.UserID, e.RequestID, eventID))
		if err != nil {
			return err
		}
	}
	expArg := w.ExpiresAt
	if w.Level == LevelObserve && w.ExpiresAt != nil {
		exp := now.Add(c.watchFor)
		if w.ExpiresAt.After(exp) {
			exp = *w.ExpiresAt
		}
		expArg = &exp
	}
	inc := 0
	if !counted {
		inc = 1
		err := tx.Model(&model.Event{}).Where("id = ?", eventID).
			Updates(map[string]any{"counted": true, "violation_epoch": w.ViolationEpoch}).Error
		if err != nil {
			return err
		}
	}
	return tx.Model(&model.Watch{}).Where("user_id = ?", e.UserID).Updates(map[string]any{"hit_count": gorm.Expr("hit_count + 1"),
		"hits_since_seen": gorm.Expr("hits_since_seen + 1"), "violation_count": gorm.Expr("violation_count + ?", inc),
		"last_hit_at": now, "expires_at": expArg, "updated_at": now}).Error
}

// applyBans bans the users whose suspicious requests since max(now -
// ban_window, last unblock, start of their last ban) reached ban_threshold.
func (p *Plugin) applyBans(ctx context.Context, users map[int64]bool) {
	c := p.cfg.Load()
	if c.BanThreshold <= 0 {
		return
	}
	db, err := p.db(ctx)
	if err != nil {
		return
	}
	now := p.now().UTC()
	changed := false
	for uid := range users {
		var n int64
		var banned bool
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := lockBlockSubject(tx, uid); err != nil {
				return err
			}
			var reached bool
			var err error
			if n, reached, err = banReached(tx, c, uid, now); err != nil || !reached {
				return err
			}
			var expires *time.Time
			if c.banDuration > 0 {
				t := now.Add(c.banDuration)
				expires = &t
			}
			reason := violationsReason(n, c.BanWindowSeconds)
			// An expired ban is replaced; a live one stays.
			res := tx.Clauses(clause.OnConflict{
				Columns:   columns("user_id"),
				DoUpdates: clause.AssignmentColumns([]string{"reason", "violations", "source", "created_at", "expires_at", "created_by"}),
				Where:     clause.Where{Exprs: []clause.Expression{gorm.Expr("blocks.expires_at IS NOT NULL AND blocks.expires_at <= ?", now)}},
			}).Create(&model.Block{UserID: uid, Reason: reason, Violations: int(n), Source: "auto", CreatedAt: now, ExpiresAt: expires})
			banned = res.RowsAffected > 0
			return res.Error
		})
		if err != nil {
			p.log.Warn("moderation: auto ban failed", "user_id", uid, "error", err.Error())
			continue
		}
		if banned {
			changed = true
			p.log.Info("moderation: user banned automatically", "user_id", uid, "violations", n)
		}
	}
	if changed {
		p.blocksChanged(ctx)
	}
}

// lockBlockSubject must precede reading the violation baseline, modifying a
// ban or a watch entry. The parent survives unblocking and cleanup.
func lockBlockSubject(tx *gorm.DB, uid int64) error {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.BlockSubject{UserID: uid}).Error; err != nil {
		return err
	}
	return tx.Clauses(forUpdate()).Where("user_id = ?", uid).Take(&model.BlockSubject{}).Error
}

// banReached counts uid's violations of the ban window (countViolations);
// reached = they reach ban_threshold (never while it is 0). The caller holds
// the user's lock.
func banReached(tx *gorm.DB, c *config, uid int64, now time.Time) (n int64, reached bool, err error) {
	if c.BanThreshold <= 0 {
		return 0, false, nil
	}
	n, err = countViolations(tx, uid, now.Add(-c.banWindow))
	return n, err == nil && n >= int64(c.BanThreshold), err
}

// countViolations counts suspicious requests (one per request id; false
// positives, replayed cached hits and 0.1 flag verdicts excluded) of uid since the later of since, the last unblock and
// the start of the user's last (expired) ban.
func countViolations(db *gorm.DB, uid int64, since time.Time) (int64, error) {
	var n int64
	err := db.Model(&model.Event{}).Select("count(DISTINCT CASE WHEN request_id = '' THEN id::text ELSE request_id END)").
		Where("user_id = ? AND review_status <> 'false_positive' AND NOT replayed AND created_at >= ?", uid, since).
		Where(suspiciousResult).Where("NOT (verdict = 'flag' AND flow_id = 0)").
		Where("created_at > coalesce((?), '-infinity'::timestamptz)", db.Model(&model.Unblock{}).Select("at").Where("user_id = ?", uid)).
		Where("created_at > coalesce((?), '-infinity'::timestamptz)", db.Model(&model.Block{}).Select("created_at").Where("user_id = ?", uid)).
		Row().Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- block list

func (p *Plugin) isBlocked(uid int64) bool {
	m := *p.blocked.Load()
	if len(m) == 0 {
		return false
	}
	exp, ok := m[uid]
	return ok && (exp.IsZero() || p.now().Before(exp))
}

func (p *Plugin) blockedCount() int {
	now := p.now()
	n := 0
	for _, exp := range *p.blocked.Load() {
		if exp.IsZero() || now.Before(exp) {
			n++
		}
	}
	return n
}

// reloadBlocks reads the active blocks into memory.
func (p *Plugin) reloadBlocks(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.blockReloadMu.Lock()
	p.blockReloadSeq++
	seq := p.blockReloadSeq
	p.blockReloadMu.Unlock()
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	var rows []model.Block
	if err := db.Select("user_id, expires_at").Where("expires_at IS NULL OR expires_at > ?", p.now().UTC()).Find(&rows).Error; err != nil {
		return err
	}
	m := make(map[int64]time.Time, len(rows))
	for _, b := range rows {
		m[b.UserID] = time.Time{}
		if b.ExpiresAt != nil {
			m[b.UserID] = *b.ExpiresAt
		}
	}
	// Never put an older snapshot back over a newer reload.
	p.blockReloadMu.Lock()
	defer p.blockReloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if seq == p.blockReloadSeq {
		p.blocked.Store(&m)
	}
	return nil
}

func (p *Plugin) kickRefresh() {
	select {
	case p.refreshNow <- struct{}{}:
	default:
	}
}

// refreshLoop reloads the block list and polls the config / watch versions
// every refreshEvery while moderation is enabled, and at once when kicked.
func (p *Plugin) refreshLoop(ctx context.Context) {
	defer p.wg.Done()
	t := time.NewTicker(p.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !p.cfg.Load().enabled {
				continue
			}
		case <-p.refreshNow:
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		p.upgradeOnce(rctx)
		p.convertParityOnce(rctx)
		if err := p.reloadBlocks(rctx); err != nil && ctx.Err() == nil {
			p.log.Debug("moderation: block list reload failed", "error", err.Error())
		}
		if err := p.pollVersions(rctx); err != nil && ctx.Err() == nil {
			p.log.Debug("moderation: config reload failed", "error", err.Error())
		}
		cancel()
	}
}

// blocksChanged reloads this node and tells the others (best effort).
func (p *Plugin) blocksChanged(ctx context.Context) {
	p.changed(ctx, TopicBlocksChanged, p.reloadBlocks)
}

func (p *Plugin) onBroadcast(reload func(context.Context) error) func(context.Context, pluginsdk.Broadcast) error {
	return func(ctx context.Context, _ pluginsdk.Broadcast) error {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return reload(rctx)
	}
}
