package egress

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// pgStore is the PostgreSQL logStore.
type pgStore struct{ db *store.DB }

// logBatch is the most rows one INSERT of log rows carries: PostgreSQL
// takes at most 65535 parameters, and a row has up to 12.
const logBatch = 65535 / 12

func (s pgStore) resetOpen(ctx context.Context, nodeID string, before time.Time) (int64, error) {
	res := resetOpenLogs(store.G(ctx, s.db.Pool), nodeID, before)
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
// rows or none (on an error no id is returned, nothing was written).
func (s pgStore) insertOpen(ctx context.Context, rows []logEntry) ([]int64, error) {
	logs := make([]model.PluginEgressLog, len(rows))
	for i, r := range rows {
		logs[i] = model.PluginEgressLog{PluginKey: r.PluginKey, NodeID: r.NodeID, Network: r.Network, Host: r.Host,
			Port: r.Port, StartedAt: r.StartedAt, Result: "open"}
	}
	if err := s.insertLogs(ctx, logs); err != nil {
		return nil, err
	}
	ids := make([]int64, len(logs))
	for i := range logs {
		ids[i] = logs[i].ID
	}
	return ids, nil
}

// insertDone inserts finished rows whole, all or none.
func (s pgStore) insertDone(ctx context.Context, rows []logEntry) error {
	logs := make([]model.PluginEgressLog, len(rows))
	for i, r := range rows {
		closed := r.StartedAt.Add(time.Duration(r.DurationMS) * time.Millisecond)
		logs[i] = model.PluginEgressLog{PluginKey: r.PluginKey, NodeID: r.NodeID, Network: r.Network, Host: r.Host,
			Port: r.Port, StartedAt: r.StartedAt, DurationMs: r.DurationMS, BytesIn: r.BytesIn, BytesOut: r.BytesOut,
			Result: r.Result, Error: r.Error, ClosedAt: &closed}
	}
	return s.insertLogs(ctx, logs)
}

// insertLogs inserts logs all or none, in multi-row INSERTs of at most
// logBatch rows (a transaction holds them when there are several), and sets
// their ids.
func (s pgStore) insertLogs(ctx context.Context, logs []model.PluginEgressLog) error {
	if len(logs) <= logBatch {
		return insertLogRows(store.G(ctx, s.db.Pool), logs).Error
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		g := store.G(ctx, tx)
		for chunk := range slices.Chunk(logs, logBatch) {
			if err := insertLogRows(g, chunk).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// insertLogRows inserts logs in one statement and sets their ids (RETURNING
// id, in row order). A zero bytes_in, bytes_out or error is written as the
// column's DEFAULT, which is that zero value.
func insertLogRows(g *gorm.DB, logs []model.PluginEgressLog) *gorm.DB {
	return g.Create(&logs)
}

func (s pgStore) closeRows(ctx context.Context, rows []closedRow) error {
	return closeLogRows(store.G(ctx, s.db.Pool), rows).Error
}

// closeLogRows updates the closed rows in one statement. Raw SQL: the
// values come in as parallel arrays through unnest.
func closeLogRows(g *gorm.DB, rows []closedRow) *gorm.DB {
	n := len(rows)
	ids, in, out := make(store.Int64s, n), make(store.Int64s, n), make(store.Int64s, n)
	results, errs := make(store.Strings, n), make(store.Strings, n)
	durations := make(store.Int64s, n) // int32 values, as the int column; $4::int[] narrows the array
	closed := make(store.Times, n)
	for i, r := range rows {
		ids[i], results[i], errs[i] = r.ID, r.Result, r.Error
		durations[i], in[i], out[i], closed[i] = int64(int32(r.DurationMS)), r.BytesIn, r.BytesOut, r.ClosedAt
	}
	return g.Exec(`
		UPDATE plugin_egress_logs l SET result = u.result, error = u.error,
			duration_ms = CASE WHEN u.duration_ms > 0 THEN u.duration_ms
				ELSE LEAST(EXTRACT(EPOCH FROM (u.closed_at - l.started_at)) * 1000, 2147483647)::int END,
			bytes_in = GREATEST(l.bytes_in, u.bytes_in), bytes_out = GREATEST(l.bytes_out, u.bytes_out),
			closed_at = u.closed_at
		FROM unnest(?::bigint[], ?::text[], ?::text[], ?::int[], ?::bigint[], ?::bigint[], ?::timestamptz[])
			AS u(id, result, error, duration_ms, bytes_in, bytes_out, closed_at)
		WHERE l.id = u.id AND l.result = 'open'`, ids, results, errs, durations, in, out, closed)
}

func (s pgStore) prune(ctx context.Context, before time.Time) error {
	return store.G(ctx, s.db.Pool).Where("started_at < ?", before).Delete(&model.PluginEgressLog{}).Error
}

func (s pgStore) upsertDomains(ctx context.Context, ds []domainDelta,
	onNew func(ctx context.Context, tx pgx.Tx, news []domainDelta) error) error {
	byKey := make(map[domainKey]domainDelta, len(ds))
	for _, d := range ds {
		byKey[domainKey{d.PluginKey, d.Host}] = d
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var rows []upsertedDomain
		if err := upsertDomainRows(store.G(ctx, tx), ds).Scan(&rows).Error; err != nil {
			return err
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

// upsertedDomain is a row upsertDomainRows returns; Inserted tells a new
// host from a known one.
type upsertedDomain struct {
	PluginKey   string    `gorm:"column:plugin_key"`
	Host        string    `gorm:"column:host"`
	FirstSeenAt time.Time `gorm:"column:first_seen_at"`
	Inserted    bool      `gorm:"column:inserted"`
}

// upsertDomainRows adds the deltas to plugin_egress_domains. Raw SQL: the
// rows come from unnest, and (xmax = 0) marks the ones the INSERT created.
func upsertDomainRows(g *gorm.DB, ds []domainDelta) *gorm.DB {
	n := len(ds)
	keys, hosts := make(store.Strings, n), make(store.Strings, n)
	first, last := make(store.Times, n), make(store.Times, n)
	conns := make(store.Int64s, n)
	for i, d := range ds {
		keys[i], hosts[i], first[i], last[i], conns[i] = d.PluginKey, d.Host, d.FirstSeen, d.LastSeen, d.Connections
	}
	return g.Raw(`
			INSERT INTO plugin_egress_domains AS d (plugin_key, host, first_seen_at, last_seen_at, connections)
			SELECT * FROM unnest(?::text[], ?::text[], ?::timestamptz[], ?::timestamptz[], ?::bigint[])
			ON CONFLICT (plugin_key, host) DO UPDATE SET
				last_seen_at = GREATEST(d.last_seen_at, EXCLUDED.last_seen_at),
				connections = d.connections + EXCLUDED.connections
			RETURNING d.plugin_key, d.host, d.first_seen_at, (xmax = 0) AS inserted`,
		keys, hosts, first, last, conns)
}
