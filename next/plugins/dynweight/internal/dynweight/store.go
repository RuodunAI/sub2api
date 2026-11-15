package dynweight

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"

	"github.com/Sub2API-Devs/sup2api/next/plugins/dynweight/internal/model"
	"github.com/Sub2API-Devs/sup2api/next/plugins/dynweight/internal/rules"
)

// key identifies the state of an (account, group, client model).
type key struct {
	AccountID int64
	GroupID   int64
	Model     string
}

func compareKeys(a, b key) int {
	return cmp.Or(cmp.Compare(a.AccountID, b.AccountID), cmp.Compare(a.GroupID, b.GroupID), strings.Compare(a.Model, b.Model))
}

// store is the plugin schema.
type store struct{ db *gorm.DB }

// with is the store's handle bound to ctx.
func (s store) with(ctx context.Context) *gorm.DB { return s.db.WithContext(ctx) }

var errVersionConflict = errors.New("the configuration was changed meanwhile")

// insertBatch is the most rows one INSERT carries (PostgreSQL takes at most
// 65535 parameters per statement).
const insertBatch = 1000

// insertAll inserts rows in batches of one transaction (all of them or
// none, like the one statement or batch it replaces), in the order given:
// callers sort them, so that concurrent writers lock rows in the same order
// and do not deadlock.
func insertAll[T any](db *gorm.DB, rows []T, clauses ...clause.Expression) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(clauses...).CreateInBatches(&rows, insertBatch).Error
	})
}

// keyColumns are the key of obs, state and sticky_pending.
var keyColumns = []clause.Column{{Name: "account_id"}, {Name: "group_id"}, {Name: "model"}}

// configStamp is the stored configuration's version (0: none stored, the
// default) and when it was saved (unix seconds, 0 for none): the console
// shows both, so an edit that was never saved is told apart from one in force.
type configStamp struct{ version, updatedAt int64 }

func stampOf(c model.Config) configStamp { return configStamp{c.Version, c.UpdatedAt.Unix()} }

// loadConfig reads the configuration; version 0 means the default. The
// stored one is read tolerantly (rules.LoadConfig); warnings say what was
// raised, fell back or was dropped.
func (s store) loadConfig(ctx context.Context) (c rules.Config, stamp configStamp, warnings []string, err error) {
	var row model.Config
	err = s.with(ctx).Take(&row, model.ConfigID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return rules.DefaultConfig(), configStamp{}, nil, nil
	}
	if err != nil {
		return rules.Config{}, configStamp{}, nil, err
	}
	if c, warnings, err = rules.LoadConfig(row.Doc); err != nil {
		return rules.Config{}, configStamp{}, nil, fmt.Errorf("stored configuration: %w", err)
	}
	return c, stampOf(row), warnings, nil
}

// saveConfig stores a normalized configuration if version is still the
// stored one (0 = none stored yet), and returns the new stamp.
func (s store) saveConfig(ctx context.Context, c rules.Config, version, userID int64) (configStamp, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return configStamp{}, err
	}
	row := model.Config{ID: model.ConfigID, Doc: raw, Version: 1, UpdatedBy: userID}
	var res *gorm.DB
	if version == 0 {
		res = s.with(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	} else {
		res = s.with(ctx).Model(&row).Clauses(clause.Returning{}).Where("version = ?", version).Updates(map[string]any{
			"doc": raw, "version": gorm.Expr("version + 1"), "updated_at": gorm.Expr("now()"), "updated_by": userID,
		})
	}
	if res.Error != nil {
		return configStamp{}, res.Error
	}
	if res.RowsAffected == 0 {
		return configStamp{}, errVersionConflict
	}
	return stampOf(row), nil
}

func (s store) configVersion(ctx context.Context) (int64, error) {
	var row model.Config
	err := s.with(ctx).Select("version").Take(&row, model.ConfigID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return row.Version, err
}

// obsDelta is counters to add to a key's window.
type obsDelta struct {
	key
	window int64
	counts rules.Counts
}

// sumCounts merges the counters of a conflicting row: every node adds its
// own, so the fields are summed.
const sumCounts = `(SELECT jsonb_object_agg(f, COALESCE((obs.counts->>f)::bigint, 0) + COALESCE((EXCLUDED.counts->>f)::bigint, 0))
	FROM (SELECT jsonb_object_keys(obs.counts) UNION SELECT jsonb_object_keys(EXCLUDED.counts)) AS u(f))`

// addObs adds counters (deltas come sorted, one per key and window).
func (s store) addObs(ctx context.Context, deltas []obsDelta) error {
	rows := make([]model.Obs, len(deltas))
	for i, d := range deltas {
		fields, err := json.Marshal(d.counts.Fields())
		if err != nil {
			return err
		}
		rows[i] = model.Obs{AccountID: d.AccountID, GroupID: d.GroupID, Model: d.Model, WindowStart: d.window, Counts: fields}
	}
	return insertAll(s.with(ctx), rows, clause.OnConflict{
		Columns:   append(slices.Clone(keyColumns), clause.Column{Name: "window_start"}),
		DoUpdates: clause.Set{{Column: clause.Column{Name: "counts"}, Value: gorm.Expr(sumCounts)}},
	})
}

// loadObs reads the counters of the keys matching f in the windows from
// since on, by key and window start.
func (s store) loadObs(ctx context.Context, f filter, since int64) (map[key]map[int64]rules.Counts, error) {
	var rows []model.Obs
	if err := s.with(ctx).Scopes(f.scope).Where("window_start >= ?", since).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[key]map[int64]rules.Counts{}
	for _, r := range rows {
		var fields map[string]int64
		if err := json.Unmarshal(r.Counts, &fields); err != nil {
			return nil, fmt.Errorf("counters %d/%d/%s: %w", r.AccountID, r.GroupID, r.Model, err)
		}
		k := key{r.AccountID, r.GroupID, r.Model}
		if out[k] == nil {
			out[k] = map[int64]rules.Counts{}
		}
		out[k][r.WindowStart] = rules.CountsOf(fields)
	}
	return out, nil
}

// stateRow is a stored state with the generation it was read at.
type stateRow struct {
	rules.State
	gen int64
}

// loadStates reads states, all of them or those matching filter.
func (s store) loadStates(ctx context.Context, f filter) (map[key]stateRow, error) {
	var rows []model.State
	if err := s.with(ctx).Select("account_id, group_id, model, data, gen").Scopes(f.scope).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[key]stateRow, len(rows))
	for _, r := range rows {
		k := key{r.AccountID, r.GroupID, r.Model}
		var st rules.State
		if err := json.Unmarshal(r.Data, &st); err != nil {
			return nil, fmt.Errorf("state %v: %w", k, err)
		}
		out[k] = stateRow{State: st, gen: r.Gen}
	}
	return out, nil
}

// stateOf is the row of a state.
func stateOf(k key, st rules.State, raw []byte) model.State {
	return model.State{AccountID: k.AccountID, GroupID: k.GroupID, Model: k.Model, Data: raw,
		Base: st.WeightBase, Eff: st.WeightEff, Tombstone: st.Tombstone, EvalWindow: st.LastEvalWindow}
}

// writeState stores a state if the stored one is still at gen (0 = none
// stored). It reports false when another writer got there first. A sticky
// clear the evaluation decided is queued in the same transaction: it exists
// exactly when its state was written.
func (s store) writeState(ctx context.Context, k key, st rules.State, gen int64, clear *pendingClear) (bool, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return false, err
	}
	if clear == nil {
		return putState(s.with(ctx), stateOf(k, st, raw), gen)
	}
	written := false
	err = s.with(ctx).Transaction(func(tx *gorm.DB) error {
		if written, err = putState(tx, stateOf(k, st, raw), gen); err != nil || !written {
			return err
		}
		return tx.Exec(`INSERT INTO sticky_pending (account_id, group_id, model, salt, percent, strategies, zero, weight)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (account_id, group_id, model) DO UPDATE SET salt = EXCLUDED.salt,
				percent = GREATEST(sticky_pending.percent, EXCLUDED.percent), strategies = EXCLUDED.strategies,
				zero = sticky_pending.zero OR EXCLUDED.zero, weight = EXCLUDED.weight, attempts = 0, created_at = now(), claimed_until = '-infinity'`,
			k.AccountID, k.GroupID, k.Model, clear.Salt, clear.Percent, clear.Strategies, clear.Zero, clear.Weight).Error
	})
	return written && err == nil, err
}

// putState inserts row (gen 0) or updates the stored one still at gen.
func putState(db *gorm.DB, row model.State, gen int64) (bool, error) {
	var res *gorm.DB
	if gen == 0 {
		res = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	} else {
		res = db.Model(&model.State{}).
			Where("account_id = ? AND group_id = ? AND model = ? AND gen = ?", row.AccountID, row.GroupID, row.Model, gen).
			Updates(map[string]any{"data": row.Data, "base": row.Base, "eff": row.Eff, "tombstone": row.Tombstone,
				"eval_window": row.EvalWindow, "gen": gorm.Expr("gen + 1"), "updated_at": gorm.Expr("now()")})
	}
	return res.RowsAffected == 1, res.Error
}

// pendingClear is a sticky clear an evaluation decided (sticky_pending): the
// key, the salt (the window start), the share, the strategies that asked
// for it (", "-joined; none for the clear of a weight dropping to 0), and
// the key's effective weight then, for the event.
type pendingClear struct {
	key
	Salt       string
	Percent    int
	Strategies string
	Zero       bool
	Weight     float64
	Attempts   int
	CreatedAt  time.Time
}

// claimClears leases up to limit pending clears no other run holds, oldest
// first, counting the attempt.
func (s store) claimClears(ctx context.Context, lease time.Duration, limit int) ([]pendingClear, error) {
	// claimed_until is not read back: '-infinity' does not fit a time.Time.
	var rows []model.StickyPending
	err := s.with(ctx).Raw(`UPDATE sticky_pending p SET claimed_until = now() + make_interval(secs => ?), attempts = p.attempts + 1
		FROM (SELECT account_id, group_id, model FROM sticky_pending WHERE claimed_until < now()
			ORDER BY created_at LIMIT ? FOR UPDATE SKIP LOCKED) c
		WHERE p.account_id = c.account_id AND p.group_id = c.group_id AND p.model = c.model
		RETURNING p.account_id, p.group_id, p.model, p.salt, p.percent, p.strategies, p.zero, p.weight, p.attempts, p.created_at`,
		lease.Seconds(), limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]pendingClear, len(rows))
	for i, r := range rows {
		out[i] = pendingClear{key: key{r.AccountID, r.GroupID, r.Model}, Salt: r.Salt, Percent: r.Percent,
			Strategies: r.Strategies, Zero: r.Zero, Weight: r.Weight, Attempts: r.Attempts, CreatedAt: r.CreatedAt}
	}
	slices.SortFunc(out, func(a, b pendingClear) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), compareKeys(a.key, b.key))
	})
	return out, nil
}

// pending selects the pending clear c, unless a newer one replaced it.
func (c pendingClear) pending(db *gorm.DB) *gorm.DB {
	return db.Model(&model.StickyPending{}).Where("account_id = ? AND group_id = ? AND model = ? AND salt = ?",
		c.AccountID, c.GroupID, c.Model, c.Salt)
}

// finishClear drops a pending clear (done, or given up) unless a newer one
// replaced it meanwhile.
func (s store) finishClear(ctx context.Context, c pendingClear) error {
	return c.pending(s.with(ctx)).Delete(&model.StickyPending{}).Error
}

// releaseClear hands a pending clear back to the next run at once.
func (s store) releaseClear(ctx context.Context, c pendingClear) error {
	return c.pending(s.with(ctx)).Update("claimed_until", gorm.Expr("'-infinity'")).Error
}

// tombstone resets keys whatever their generation: all of them or none.
// window gives a key's reset window.
func (s store) tombstone(ctx context.Context, keys []key, window func(key) int64) error {
	rows := make([]model.State, len(keys))
	// In key order, like the counters, so concurrent resets do not deadlock.
	for i, k := range slices.SortedFunc(slices.Values(keys), compareKeys) {
		w := window(k)
		st := rules.Tombstone(w)
		raw, err := json.Marshal(st)
		if err != nil {
			return err
		}
		rows[i] = stateOf(k, st, raw)
		// The reset columns, whatever the document says.
		rows[i].Base, rows[i].Eff, rows[i].Tombstone, rows[i].EvalWindow = 0, 0, true, w
	}
	return insertAll(s.with(ctx), rows, clause.OnConflict{
		Columns: keyColumns,
		DoUpdates: append(clause.AssignmentColumns([]string{"data", "base", "eff", "tombstone", "eval_window"}),
			clause.Assignments(map[string]any{"gen": gorm.Expr("state.gen + 1"), "updated_at": gorm.Expr("now()")})...),
	})
}

// weight is the effective weight of a key that is not at its base, and
// when scheduling last offered its account in its group (the epoch when
// unknown).
type weight struct {
	base   int
	eff    float64
	seenAt time.Time
}

// loadWeights reads the keys whose effective weight is not their base:
// everything scheduling needs.
func (s store) loadWeights(ctx context.Context) (map[key]weight, error) {
	var rows []struct {
		model.State
		SeenAt time.Time
	}
	err := s.with(ctx).Raw(`SELECT s.account_id, s.group_id, s.model, s.base, s.eff, COALESCE(b.seen_at, 'epoch') AS seen_at
		FROM state s LEFT JOIN bases b ON b.account_id = s.account_id AND b.group_id = s.group_id
		WHERE NOT s.tombstone AND s.eff <> s.base`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[key]weight, len(rows))
	for _, r := range rows {
		out[key{r.AccountID, r.GroupID, r.Model}] = weight{base: r.Base, eff: r.Eff, seenAt: r.SeenAt}
	}
	return out, nil
}

// loadTrends reads what rules.Trend needs of the degraded keys of the
// accounts, in one query (the partial index on eff <> base): GET /weights
// asks for a page of the account list at once.
func (s store) loadTrends(ctx context.Context, accountIDs []int64) (map[key]rules.State, error) {
	var rows []struct {
		model.State
		LastChange   string
		LastChangeAt int64
	}
	err := s.with(ctx).Raw(`SELECT account_id, group_id, model, base, eff, COALESCE(data->>'last_change', '') AS last_change,
		COALESCE((data->>'last_change_at')::bigint, 0) AS last_change_at
		FROM state WHERE account_id = ANY(?) AND NOT tombstone AND eff <> base`, pgdb.Array[int64](accountIDs)).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[key]rules.State, len(rows))
	for _, r := range rows {
		out[key{r.AccountID, r.GroupID, r.Model}] = rules.State{WeightBase: r.Base, WeightEff: r.Eff, LastChange: r.LastChange, LastChangeAt: r.LastChangeAt}
	}
	return out, nil
}

// base is the weight an account has in a group, as scheduling saw it.
type base struct {
	Weight int
	Name   string
	SeenAt time.Time
}

type pair struct{ AccountID, GroupID int64 }

func comparePairs(a, b pair) int {
	return cmp.Or(cmp.Compare(a.AccountID, b.AccountID), cmp.Compare(a.GroupID, b.GroupID))
}

func (s store) loadBases(ctx context.Context) (map[pair]base, error) {
	var rows []model.Base
	if err := s.with(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[pair]base, len(rows))
	for _, r := range rows {
		out[pair{r.AccountID, r.GroupID}] = base{Weight: r.Weight, Name: r.Name, SeenAt: r.SeenAt}
	}
	return out, nil
}

func (s store) saveBases(ctx context.Context, bases map[pair]base) error {
	rows := make([]model.Base, 0, len(bases))
	// In pair order, like the counters, so concurrent writers do not deadlock.
	for _, p := range slices.SortedFunc(maps.Keys(bases), comparePairs) {
		v := bases[p]
		rows = append(rows, model.Base{AccountID: p.AccountID, GroupID: p.GroupID, Weight: v.Weight, Name: v.Name, SeenAt: v.SeenAt})
	}
	return insertAll(s.with(ctx), rows, clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "group_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"weight", "name", "seen_at"}),
	})
}

// eventRow is an event of a key. configVersion is the configuration
// version the evaluation ran on, for the events the configuration decided
// (rules.Event.ByConfig; nil for the others), so an event shows whether a
// save was in force. It goes into the stored detail only, like the window
// and the steps: the log line and the notification stay as they were.
type eventRow struct {
	key
	rules.Event
	Detail        map[string]any
	configVersion *int64
}

func (s store) addEvents(ctx context.Context, events []eventRow) error {
	rows := make([]model.Event, len(events))
	for i, e := range events {
		detail := maps.Clone(e.Detail)
		if detail == nil {
			detail = map[string]any{}
		}
		if e.Window != nil {
			detail["window"] = e.Window
		}
		if len(e.Steps) > 0 {
			detail["steps"] = e.Steps
		}
		if e.configVersion != nil {
			detail["config_version"] = *e.configVersion
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		names := e.Rules
		if names == nil {
			names = []string{}
		}
		rows[i] = model.Event{AccountID: e.AccountID, GroupID: e.GroupID, Model: e.Model, Kind: e.Kind, Reason: e.Reason,
			WeightFrom: e.From, WeightTo: e.To, Rules: names, Detail: raw}
	}
	return insertAll(s.with(ctx), rows)
}

// keyColumnList selects the key columns.
const keyColumnList = "account_id, group_id, model"

// keys lists the keys matching f with a state, or with counters from since on.
func (s store) keys(ctx context.Context, f filter, since int64) ([]key, error) {
	db := s.with(ctx)
	states := db.Model(&model.State{}).Select(keyColumnList).Scopes(f.scope)
	obs := db.Model(&model.Obs{}).Select(keyColumnList).Scopes(f.scope).Where("window_start >= ?", since)
	var rows []model.State
	if err := db.Raw("? UNION ?", states, obs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]key, len(rows))
	for i, r := range rows {
		out[i] = key{r.AccountID, r.GroupID, r.Model}
	}
	return out, nil
}

// listEvents pages the events matching f (and kind), newest first.
func (s store) listEvents(ctx context.Context, f filter, kind string, limit, offset int) ([]eventItem, int64, error) {
	match := func(db *gorm.DB) *gorm.DB {
		db = db.Model(&model.Event{}).Scopes(f.scope)
		if kind != "" {
			db = db.Where("kind = ?", kind)
		}
		return db
	}
	var total int64
	if err := s.with(ctx).Scopes(match).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []struct {
		model.Event
		AccountName string
	}
	// The account's name as scheduling last saw it, in any group.
	err := s.with(ctx).Scopes(match).Select(`events.*, COALESCE((SELECT b.name FROM bases b
			WHERE b.account_id = events.account_id ORDER BY b.seen_at DESC LIMIT 1), '') AS account_name`).
		Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	items := make([]eventItem, len(rows))
	for i, r := range rows {
		items[i] = eventItem{ID: r.ID, CreatedAt: r.CreatedAt, AccountID: r.AccountID, AccountName: r.AccountName,
			GroupID: r.GroupID, Model: r.Model, Kind: r.Kind, Reason: r.Reason, From: r.WeightFrom, To: r.WeightTo, Rules: r.Rules}
		if err := json.Unmarshal(r.Detail, &items[i].Detail); err != nil {
			return nil, 0, fmt.Errorf("event %d: %w", r.ID, err)
		}
	}
	return items, total, nil
}

// filter narrows states and events to accounts, a group and a model (zero
// values match everything).
type filter struct {
	AccountID  int64   `json:"account_id"`
	AccountIDs []int64 `json:"account_ids"` // POST /reset only: AccountID or any of these
	GroupID    int64   `json:"group_id"`
	Model      string  `json:"model"`
}

// accounts lists the accounts f narrows to, none for every account.
func (f filter) accounts() []int64 {
	ids := slices.DeleteFunc(slices.Clone(f.AccountIDs), func(id int64) bool { return id <= 0 })
	if f.AccountID > 0 {
		ids = append(ids, f.AccountID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// scope narrows a query on a table with the key columns to f.
func (f filter) scope(db *gorm.DB) *gorm.DB {
	if ids := f.accounts(); len(ids) > 0 {
		db = db.Where("account_id = ANY(?)", pgdb.Array[int64](ids))
	}
	if f.GroupID > 0 {
		db = db.Where("group_id = ?", f.GroupID)
	}
	if f.Model != "" {
		db = db.Where("model = ?", f.Model)
	}
	return db
}

// oldObs selects the counters of the windows before since.
func oldObs(db *gorm.DB, since int64) *gorm.DB {
	return db.Model(&model.Obs{}).Where("window_start < ?", since)
}

// pruneObs drops the counters older than since (obsSince): the evaluate job
// does it every run, the cleanup again with the rest.
func (s store) pruneObs(ctx context.Context, since int64) error {
	return oldObs(s.with(ctx), since).Delete(&model.Obs{}).Error
}

// prune deletes the counters older than since, the events older than
// eventsBefore, the states with nothing left to evaluate (at the base or
// reset, without counters since), the bases and states of pairs scheduling
// has not seen since staleBefore, and the pending sticky clears queued
// before clearsBefore (the evaluate job gives up on them much sooner; this
// only catches what a crash left).
func (s store) prune(ctx context.Context, since int64, eventsBefore, staleBefore, clearsBefore time.Time) (int64, error) {
	db := s.with(ctx)
	var n int64
	for _, q := range []struct {
		model any
		where *gorm.DB
	}{
		{&model.Obs{}, oldObs(db, since)},
		{&model.Event{}, db.Where("created_at < ?", eventsBefore)},
		{&model.State{}, db.Where(`(tombstone OR eff = base) AND eval_window < ? AND NOT EXISTS (
			SELECT 1 FROM obs o WHERE o.account_id = state.account_id AND o.group_id = state.group_id AND o.model = state.model)`, since)},
		{&model.State{}, db.Where(`NOT EXISTS (SELECT 1 FROM bases b WHERE b.account_id = state.account_id AND b.group_id = state.group_id AND b.seen_at >= ?)`, staleBefore)},
		{&model.Base{}, db.Where("seen_at < ?", staleBefore)},
		{&model.StickyPending{}, db.Where("created_at < ?", clearsBefore)},
	} {
		res := q.where.Delete(q.model)
		if res.Error != nil {
			return n, res.Error
		}
		n += res.RowsAffected
	}
	return n, nil
}
