package egress

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// pgStore is the PostgreSQL logStore.
type pgStore struct{ db *store.DB }

func (s pgStore) resetOpen(ctx context.Context, nodeID string, before time.Time) (int64, error) {
	res := resetOpenLogs(store.G(ctx, s.db.Gorm), nodeID, before)
	return res.RowsAffected, res.Error
}

// resetOpenLogs closes the rows a node left open when it went away.
func resetOpenLogs(g *gorm.DB, nodeID string, before time.Time) *gorm.DB {
	return g.Model(&model.PluginEgressLog{}).
		Where("result = 'open' AND node_id = ? AND started_at < ?", nodeID, before).
		Updates(map[string]any{
			"result": "reset", "error": "node restarted", "closed_at": gorm.Expr("now()"),
			"duration_ms": gorm.Expr("LEAST(EXTRACT(EPOCH FROM (now() - started_at)) * 1000, 2147483647)::int"),
		})
}

// insertOpen inserts result='open' rows and returns their ids in order, all
// rows or none (on an error no id is returned, nothing was written). The ids
// are reserved first (store.NextIDs), so each is its row's whatever order
// the INSERT stores the rows in.
func (s pgStore) insertOpen(ctx context.Context, rows []logEntry) ([]int64, error) {
	ids, err := store.NextIDs(store.G(ctx, s.db.Gorm), model.PluginEgressLog{}.TableName(), len(rows))
	if err != nil {
		return nil, err
	}
	open := make([]logRow, len(rows))
	for i, r := range rows {
		open[i] = logRow{ID: ids[i], PluginKey: r.PluginKey, NodeID: r.NodeID, Network: r.Network,
			Host: r.Host, Port: r.Port, StartedAt: r.StartedAt, Result: ResultOpen}
	}
	if err := s.create(ctx, open); err != nil {
		return nil, err
	}
	return ids, nil
}

// insertDone inserts finished rows whole, all or none, their ids from the
// sequence. A finished row's closed_at is its start plus its duration.
func (s pgStore) insertDone(ctx context.Context, rows []logEntry) error {
	done := make([]logRow, len(rows))
	for i, r := range rows {
		closed := r.StartedAt.Add(time.Duration(r.DurationMS) * time.Millisecond)
		done[i] = logRow{PluginKey: r.PluginKey, NodeID: r.NodeID, Network: r.Network, Host: r.Host,
			Port: r.Port, StartedAt: r.StartedAt, DurationMs: int(int32(r.DurationMS)), BytesIn: r.BytesIn,
			BytesOut: r.BytesOut, Result: r.Result, Error: r.Error, ClosedAt: &closed}
	}
	return s.create(ctx, done, "id")
}

// logRow is a plugin_egress_logs row the writer inserts, every column
// written as given. model.PluginEgressLog would write a column with a
// database default only when some row's value is not zero, and read it
// back (RETURNING): the INSERT's text would follow the values.
type logRow struct {
	ID         int64      `gorm:"column:id;primaryKey;autoIncrement:false"`
	PluginKey  string     `gorm:"column:plugin_key"`
	NodeID     string     `gorm:"column:node_id"`
	Network    string     `gorm:"column:network"`
	Host       string     `gorm:"column:host"`
	Port       int        `gorm:"column:port"`
	StartedAt  time.Time  `gorm:"column:started_at"`
	DurationMs int        `gorm:"column:duration_ms"`
	BytesIn    int64      `gorm:"column:bytes_in"`
	BytesOut   int64      `gorm:"column:bytes_out"`
	Result     string     `gorm:"column:result"`
	Error      string     `gorm:"column:error"`
	ClosedAt   *time.Time `gorm:"column:closed_at"`
}

func (logRow) TableName() string { return model.PluginEgressLog{}.TableName() }

// create writes rows without the omitted columns, all or none:
// store.InsertChunk rows per INSERT, in a transaction when they take more
// than one, so a full chunk has one text whatever the batch size.
func (s pgStore) create(ctx context.Context, rows []logRow, omit ...string) error {
	if len(rows) == 0 {
		return nil
	}
	insert := func(g *gorm.DB) error { return g.Omit(omit...).CreateInBatches(&rows, store.InsertChunk).Error }
	if len(rows) <= store.InsertChunk {
		return insert(store.G(ctx, s.db.Gorm))
	}
	return s.db.Tx(ctx, func(tx *gorm.DB) error { return insert(store.G(ctx, tx)) })
}

func (s pgStore) closeRows(ctx context.Context, rows []closedRow) error {
	return closeLogRows(store.G(ctx, s.db.Gorm), rows).Error
}

// closeLogRows updates the closed rows in one statement: their values come
// in as parallel arrays, turned into rows by unnest (store.UpdateFrom).
func closeLogRows(g *gorm.DB, rows []closedRow) *gorm.DB {
	n := len(rows)
	ids, in, out := make(store.Int64s, n), make(store.Int64s, n), make(store.Int64s, n)
	results, errs := make(store.Strings, n), make(store.Strings, n)
	durations := make(store.Int64s, n) // int32 values, as the int column; ::int[] narrows the array
	closed := make(store.Times, n)
	for i, r := range rows {
		ids[i], results[i], errs[i] = r.ID, r.Result, r.Error
		durations[i], in[i], out[i], closed[i] = int64(int32(r.DurationMS)), r.BytesIn, r.BytesOut, r.ClosedAt
	}
	return g.Table("plugin_egress_logs AS l").
		Clauses(store.UpdateFrom(`unnest(?::bigint[], ?::text[], ?::text[], ?::int[], ?::bigint[], ?::bigint[], ?::timestamptz[])
			AS u(id, result, error, duration_ms, bytes_in, bytes_out, closed_at)`, ids, results, errs, durations, in, out, closed)).
		Where("l.id = u.id AND l.result = 'open'").
		Updates(map[string]any{
			"result": gorm.Expr("u.result"), "error": gorm.Expr("u.error"),
			"duration_ms": gorm.Expr(`CASE WHEN u.duration_ms > 0 THEN u.duration_ms
				ELSE LEAST(EXTRACT(EPOCH FROM (u.closed_at - l.started_at)) * 1000, 2147483647)::int END`),
			"bytes_in": gorm.Expr("GREATEST(l.bytes_in, u.bytes_in)"), "bytes_out": gorm.Expr("GREATEST(l.bytes_out, u.bytes_out)"),
			"closed_at": gorm.Expr("u.closed_at"),
		})
}

func (s pgStore) prune(ctx context.Context, before time.Time) error {
	return store.G(ctx, s.db.Gorm).Where("started_at < ?", before).Delete(&model.PluginEgressLog{}).Error
}

func (s pgStore) upsertDomains(ctx context.Context, ds []domainDelta,
	onNew func(ctx context.Context, tx *gorm.DB, news []domainDelta) error) error {
	byKey := make(map[domainKey]domainDelta, len(ds))
	for _, d := range ds {
		byKey[domainKey{d.PluginKey, d.Host}] = d
	}
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		rows, res := upsertDomainRows(store.G(ctx, tx), ds)
		if res.Error != nil {
			return res.Error
		}
		var news []domainDelta
		for _, r := range rows {
			if r.Inserted {
				d := byKey[domainKey{r.PluginKey, r.Host}]
				d.FirstSeen = r.FirstSeenAt
				news = append(news, d)
			}
		}
		if len(news) == 0 || onNew == nil {
			return nil
		}
		return onNew(ctx, tx, news)
	})
}

// upsertedDomain is a plugin_egress_domains row upsertDomainRows writes,
// and then the row it wrote: RETURNING reads the key, the first sighting
// and whether the INSERT created it (xmax = 0) back into it. Each returned
// row carries its own key, so it does not matter which input it lands on.
type upsertedDomain struct {
	PluginKey   string    `gorm:"column:plugin_key"`
	Host        string    `gorm:"column:host"`
	FirstSeenAt time.Time `gorm:"column:first_seen_at"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at"`
	Connections int64     `gorm:"column:connections"`
	Inserted    bool      `gorm:"column:inserted;->"`
}

func (upsertedDomain) TableName() string { return model.PluginEgressDomain{}.TableName() }

// upsertDomainRows adds the deltas to plugin_egress_domains and reads the
// rows back into the returned slice, store.InsertChunk rows per statement
// (in the caller's transaction): any number of hosts stays under
// PostgreSQL's 65535 parameters.
func upsertDomainRows(g *gorm.DB, ds []domainDelta) ([]upsertedDomain, *gorm.DB) {
	rows := make([]upsertedDomain, len(ds))
	for i, d := range ds {
		rows[i] = upsertedDomain{PluginKey: d.PluginKey, Host: d.Host, FirstSeenAt: d.FirstSeen, LastSeenAt: d.LastSeen, Connections: d.Connections}
	}
	return rows, g.Clauses(
		clause.OnConflict{Columns: []clause.Column{{Name: "plugin_key"}, {Name: "host"}}, DoUpdates: clause.Set{
			{Column: clause.Column{Name: "last_seen_at"}, Value: gorm.Expr("GREATEST(plugin_egress_domains.last_seen_at, EXCLUDED.last_seen_at)")},
			{Column: clause.Column{Name: "connections"}, Value: gorm.Expr("plugin_egress_domains.connections + EXCLUDED.connections")},
		}},
		clause.Returning{Columns: []clause.Column{{Name: "plugin_key"}, {Name: "host"}, {Name: "first_seen_at"}, {Name: "(xmax = 0) AS inserted", Raw: true}}},
	).CreateInBatches(&rows, store.InsertChunk)
}
