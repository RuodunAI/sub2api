package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const maxErrorLen = 2000

// worker delivers events to one plugin. It runs on every node; only the
// holder of lock events:{plugin} does any work.
type worker struct {
	s      *Service
	key    string
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu      sync.Mutex
	binding core.SubscriptionBinding
	match   matcher

	// owned by the run goroutine
	lock core.Lock
	gap  *gapState
}

// gapState tracks the first hole in the id sequence after the cursor. A
// hole is an id allocated by a transaction that has not committed (yet) or
// rolled back; delivering past it too early would lose that event.
type gapState struct {
	start    int64     // first missing id
	seenAt   time.Time // when the hole was first observed
	xmax     uint64    // pg snapshot xmax at first observation
	resolved bool
}

func newWorker(s *Service, b core.SubscriptionBinding) *worker {
	ctx, cancel := context.WithCancel(s.ctx)
	w := &worker{s: s, key: b.Plugin.Key, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	w.setBinding(b)
	return w
}

func (w *worker) setBinding(b core.SubscriptionBinding) {
	w.mu.Lock()
	w.binding = b
	w.match = newMatcher(b.Events.Subscribe)
	w.mu.Unlock()
	w.poke()
}

func (w *worker) current() (core.SubscriptionBinding, matcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.binding, w.match
}

func (w *worker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) run() {
	defer w.dropLock()
	poll := w.s.opts.PollInterval
	for {
		if w.ctx.Err() != nil {
			return
		}
		wait := poll
		if w.holdLock() {
			// The step never outlives the lease, so two nodes can not call
			// the plugin at the same time.
			sctx, cancel := context.WithDeadline(w.ctx, w.lock.Until())
			d, err := w.step(sctx)
			cancel()
			if err != nil {
				if w.ctx.Err() != nil {
					return
				}
				w.s.log.Warn("event delivery step failed", "plugin", w.key, "err", err)
				d = poll
			}
			wait = d
		}
		if wait <= 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-w.ctx.Done():
			t.Stop()
			return
		case <-w.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// holdLock makes sure this node holds events:{plugin} with enough validity
// left for one full step. A held lock is extended in place between steps, so
// it never lapses while this node is delivering and no other node can take
// over mid-stream (which would redeliver the batch in flight). It is only
// taken again from scratch when the extend says it was lost. The cursor is
// committed before the next step either way, so a takeover stays safe.
func (w *worker) holdLock() bool {
	if w.s.opts.CanRun != nil && !w.s.opts.CanRun() {
		w.dropLock()
		return false
	}
	o := w.s.opts
	need := o.stepValidity()
	if w.lock != nil {
		if time.Until(w.lock.Until()) > need {
			return true
		}
		ok, err := w.lock.Extend(w.ctx)
		if ok && time.Until(w.lock.Until()) > need {
			return true
		}
		if err != nil && w.ctx.Err() == nil {
			w.s.log.Warn("event delivery lock extend failed", "plugin", w.key, "err", err)
		}
		w.dropLock()
	}
	lk, ok, err := w.s.locker.TryLock(w.ctx, "events:"+w.key, o.LockTTL)
	if err != nil {
		if w.ctx.Err() == nil {
			w.s.log.Warn("event delivery lock failed", "plugin", w.key, "err", err)
		}
		return false
	}
	if !ok {
		return false
	}
	w.lock = lk
	return true
}

func (w *worker) dropLock() {
	if w.lock != nil {
		w.lock.Release()
		w.lock = nil
	}
}

type cursor struct {
	last      int64
	failures  int
	nextRetry *time.Time
}

type outboxRow struct {
	id  int64
	typ string
}

// step runs one delivery attempt and returns how long to wait before the
// next one (0 = immediately).
func (w *worker) step(ctx context.Context) (time.Duration, error) {
	o := w.s.opts
	b, m := w.current()

	cur, err := w.loadCursor(ctx)
	if err != nil {
		return 0, err
	}
	if cur.nextRetry != nil {
		if d := time.Until(*cur.nextRetry); d > 0 {
			return min(d, o.PollInterval), nil
		}
	}
	if w.gap != nil && !w.gap.resolved {
		if err := w.checkGap(ctx); err != nil {
			return 0, err
		}
	}

	rows, err := w.scan(ctx, cur.last, o.ScanLimit)
	if err != nil {
		return 0, err
	}
	batchSize := b.Events.BatchSize
	if batchSize <= 0 {
		batchSize = o.BatchSize
	}
	var ids []int64
	horizon := cur.last // highest id known to be settled (delivered or not matching)
	blocked := false
	expected := cur.last + 1
	for _, r := range rows {
		if r.id != expected {
			ok, err := w.gapPassable(ctx, expected)
			if err != nil {
				return 0, err
			}
			if !ok {
				blocked = true
				break
			}
		}
		horizon, expected = r.id, r.id+1
		if m.match(r.typ) {
			ids = append(ids, r.id)
			if len(ids) >= batchSize {
				break
			}
		}
	}
	more := !blocked && (len(rows) >= o.ScanLimit || len(ids) >= batchSize)

	if len(ids) == 0 {
		if horizon > cur.last {
			if err := w.advance(ctx, cur.last, horizon); err != nil {
				return 0, err
			}
		}
		if more {
			return 0, nil
		}
		return o.PollInterval, nil
	}

	events, err := w.load(ctx, ids)
	if err != nil {
		return 0, err
	}
	batchMax := ids[len(ids)-1]

	cctx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	resp, callErr := b.Client.OnEvents(cctx, &pluginv1.OnEventsRequest{Events: events})
	cancel()
	if ctx.Err() != nil {
		return 0, ctx.Err() // stopping or lease lost: the batch will be redelivered
	}
	if callErr == nil {
		acked := resp.GetAckedThroughId()
		if acked > cur.last {
			next := acked
			if acked >= batchMax {
				next = horizon
			}
			if err := w.advance(ctx, cur.last, next); err != nil {
				return 0, err
			}
			return 0, nil
		}
		callErr = fmt.Errorf("plugin acknowledged through id %d, not past cursor %d", acked, cur.last)
	}
	return w.fail(ctx, cur, ids[0], batchMax, horizon, callErr)
}

// fail records a failed batch: backoff, or dead-letter after MaxFailures.
func (w *worker) fail(ctx context.Context, cur cursor, first, last, horizon int64, cause error) (time.Duration, error) {
	o := w.s.opts
	msg := truncate(cause.Error())
	failures := cur.failures + 1
	if failures >= o.MaxFailures {
		w.s.log.Error("event batch dead-lettered", "plugin", w.key, "first_event_id", first, "last_event_id", last, "err", msg)
		err := w.s.db.Tx(ctx, func(tx *gorm.DB) error {
			dead := model.PluginEventDeadletter{PluginKey: w.key, FirstEventID: first, LastEventID: last, Error: msg}
			if err := store.G(ctx, tx).Create(&dead).Error; err != nil {
				return err
			}
			return moveCursor(store.G(ctx, tx), w.key, cur.last, map[string]any{
				"last_event_id": horizon, "consecutive_failures": 0, "next_retry_at": nil, "last_error": msg,
			})
		})
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	delay := backoff(failures, o.BackoffMin, o.BackoffMax)
	w.s.log.Warn("event delivery failed", "plugin", w.key, "failures", failures, "retry_in", delay, "err", msg)
	next := time.Now().Add(delay)
	err := updateCursor(store.G(ctx, w.s.db.Gorm), w.key, cur.last, map[string]any{
		"consecutive_failures": failures, "next_retry_at": next, "last_error": msg,
	}).Error
	if err != nil {
		return 0, err
	}
	return min(delay, o.PollInterval), nil
}

var errCursorMoved = errors.New("cursor moved concurrently")

// advance moves the cursor forward and clears the failure state. The update
// is conditional on the cursor we read, so a node that lost its lease
// mid-step can never move the cursor backwards.
func (w *worker) advance(ctx context.Context, from, to int64) error {
	return moveCursor(store.G(ctx, w.s.db.Gorm), w.key, from, map[string]any{
		"last_event_id": to, "consecutive_failures": 0, "next_retry_at": nil, "last_error": "",
	})
}

// updateCursor writes set, and updated_at, to the cursor of key if it still
// stands at from. set is a map because a struct would drop its zero values
// (0, NULL, the empty string), which are exactly what a reset writes.
func updateCursor(g *gorm.DB, key string, from int64, set map[string]any) *gorm.DB {
	set["updated_at"] = gorm.Expr("now()")
	return g.Model(&model.PluginEventCursor{}).Where("plugin_key = ? AND last_event_id = ?", key, from).Updates(set)
}

// moveCursor is updateCursor for a write that must land: errCursorMoved
// when the cursor is no longer at from.
func moveCursor(g *gorm.DB, key string, from int64, set map[string]any) error {
	res := updateCursor(g, key, from, set)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errCursorMoved
	}
	return nil
}

// loadCursor reads the cursor, creating it at the current max event id for
// a new subscription (history is not replayed).
func (w *worker) loadCursor(ctx context.Context) (cursor, error) {
	c, err := w.readCursor(ctx)
	if store.IsNoRows(err) {
		if err := createCursor(store.G(ctx, w.s.db.Gorm), w.key).Error; err != nil {
			return c, fmt.Errorf("create cursor: %w", err)
		}
		c, err = w.readCursor(ctx)
	}
	return c, err
}

// createCursor inserts the cursor of key at the newest event, unless another
// node just did.
func createCursor(g *gorm.DB, key string) *gorm.DB {
	return store.InsertSelect(g, model.PluginEventCursor{}.TableName(), []string{"plugin_key", "last_event_id"},
		g.Model(&model.Event{}).Select("?::text, COALESCE(max(id), 0)", key),
		clause.OnConflict{Columns: []clause.Column{{Name: "plugin_key"}}, DoNothing: true})
}

// readCursor reads the cursor row; a missing row is a no-rows error.
func (w *worker) readCursor(ctx context.Context) (cursor, error) {
	var row model.PluginEventCursor
	err := store.G(ctx, w.s.db.Gorm).Select("last_event_id", "consecutive_failures", "next_retry_at").
		Where("plugin_key = ?", w.key).Take(&row).Error
	return cursor{last: row.LastEventID, failures: row.ConsecutiveFailures, nextRetry: row.NextRetryAt}, err
}

func (w *worker) scan(ctx context.Context, after int64, limit int) ([]outboxRow, error) {
	var rows []model.Event
	if err := store.G(ctx, w.s.db.Gorm).Select("id", "type").Where("id > ?", after).
		Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]outboxRow, len(rows))
	for i, r := range rows {
		out[i] = outboxRow{id: r.ID, typ: r.Type}
	}
	return out, nil
}

func (w *worker) load(ctx context.Context, ids []int64) ([]*pluginv1.Event, error) {
	var rows []model.Event
	if err := loadEvents(store.G(ctx, w.s.db.Gorm), ids, &rows).Error; err != nil {
		return nil, err
	}
	out := make([]*pluginv1.Event, len(rows))
	for i, r := range rows {
		// The bridge reads jsonb in its text form: the text payload::text gave.
		out[i] = &pluginv1.Event{Id: r.ID, Type: r.Type, OccurredAtUnixMs: r.OccurredAt.UnixMilli(), PayloadJson: string(r.Payload)}
	}
	return out, nil
}

// loadEvents reads the events ids, in id order.
func loadEvents(g *gorm.DB, ids []int64, rows *[]model.Event) *gorm.DB {
	return g.Where("id = ANY(?)", store.Int64s(ids)).Order("id").Find(rows)
}

// gapPassable reports whether the hole starting at id start may be skipped.
// A new hole is recorded and blocks until checkGap resolves it on a later
// step (the check must run before the scan that skips the hole, otherwise a
// row committed in between could be missed).
func (w *worker) gapPassable(ctx context.Context, start int64) (bool, error) {
	if w.gap != nil && w.gap.start == start {
		return w.gap.resolved, nil
	}
	_, xmax, err := w.snapshot(ctx)
	if err != nil {
		return false, err
	}
	w.gap = &gapState{start: start, seenAt: time.Now(), xmax: xmax}
	return false, nil
}

// checkGap resolves the recorded hole once every transaction that was
// running when it was observed has finished (so the missing id can never
// appear), or after GapMaxWait.
func (w *worker) checkGap(ctx context.Context) error {
	g := w.gap
	age := time.Since(g.seenAt)
	if age < w.s.opts.GapMinWait {
		return nil
	}
	if age >= w.s.opts.GapMaxWait {
		w.s.log.Warn("skipping event id gap after max wait; a late commit would not be delivered",
			"plugin", w.key, "event_id", g.start, "waited", age)
		g.resolved = true
		return nil
	}
	xmin, _, err := w.snapshot(ctx)
	if err != nil {
		return err
	}
	g.resolved = xmin >= g.xmax
	return nil
}

func (w *worker) snapshot(ctx context.Context) (xmin, xmax uint64, err error) {
	return store.CurrentSnapshot(store.G(ctx, w.s.db.Gorm))
}

func backoff(failures int, lo, hi time.Duration) time.Duration {
	d := lo
	for i := 1; i < failures && d < hi; i++ {
		d *= 2
	}
	return min(d, hi)
}

// truncate caps an error message at maxErrorLen bytes. It is the fixed-length
// variant of x.Truncate (no rune-boundary handling, no ellipsis): the length
// is part of what subscribers see, so it stays byte-exact.
func truncate(s string) string {
	if len(s) > maxErrorLen {
		return s[:maxErrorLen]
	}
	return s
}

// matcher implements subscription patterns: exact type, "prefix.*" or "*".
type matcher struct {
	all      bool
	exact    map[string]bool
	prefixes []string
}

func newMatcher(patterns []string) matcher {
	m := matcher{exact: map[string]bool{}}
	for _, p := range patterns {
		switch {
		case p == "*":
			m.all = true
		case strings.HasSuffix(p, ".*"):
			m.prefixes = append(m.prefixes, strings.TrimSuffix(p, "*"))
		case p != "":
			m.exact[p] = true
		}
	}
	return m
}

func (m matcher) match(typ string) bool {
	if m.all || m.exact[typ] {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}
