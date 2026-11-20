package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"

	"github.com/Sub2API-Devs/sup2api/next/plugins/guard/internal/model"
)

// Job ids declared in manifest.json.
const (
	JobRollup  = "rollup"
	JobCleanup = "cleanup"
)

// Retention used by the cleanup job.
const (
	minutelyRetention = 8 * 24 * time.Hour
	longRetention     = 30 * 24 * time.Hour
	rollupLookback    = 26 * time.Hour
)

// usageRecorded is the part of the usage.recorded payload guard uses
// (CONTRACTS §6).
type usageRecorded struct {
	GroupID   int64     `json:"group_id"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}

// OnEvents implements pluginsdk.EventHandler. Each event is processed once:
// processed_events guards against redelivery, in the same transaction as the
// counter update.
func (p *Plugin) OnEvents(ctx context.Context, in *pluginv1.OnEventsRequest) (*pluginv1.OnEventsResponse, error) {
	evs := in.GetEvents()
	if len(evs) == 0 {
		return &pluginv1.OnEventsResponse{}, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	var last int64
	err = db.Transaction(func(tx *gorm.DB) error {
		for _, ev := range evs {
			if err := p.applyEvent(tx, ev); err != nil {
				return err
			}
			if ev.GetId() > last {
				last = ev.GetId()
			}
		}
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Unavailable, fmt.Sprintf("guard: store events: %v", err))
	}
	return &pluginv1.OnEventsResponse{AckedThroughId: last}, nil
}

// applyEvent records one event as processed and counts it. The idempotency
// insert and the counter update share the caller's transaction, so a
// redelivered event is either fully applied or not at all.
func (p *Plugin) applyEvent(tx *gorm.DB, ev *pluginv1.Event) error {
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ProcessedEvent{EventID: ev.GetId()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 || ev.GetType() != "usage.recorded" { // 0: applied before
		return nil
	}
	var u usageRecorded
	if err := json.Unmarshal([]byte(ev.GetPayloadJson()), &u); err != nil {
		p.log.Warn("guard: bad usage.recorded payload", "event_id", ev.GetId(), "error", err.Error())
		return nil
	}
	at := u.CreatedAt
	if at.IsZero() {
		at = time.UnixMilli(ev.GetOccurredAtUnixMs())
	}
	return addStats(tx, []model.StatsMinutely{{Minute: minuteOf(at), GroupID: u.GroupID, Model: u.Model, Requests: 1}})
}

// minuteOf is the start of t's minute, as date_trunc('minute', t) (time
// zone offsets are whole minutes; the driver truncates t to microseconds).
func minuteOf(t time.Time) time.Time { return t.UTC().Truncate(time.Minute) }

// addStats adds the counters of rows (one per minute, group and model) to
// stats_minutely, in the order given: the order the rows are locked in.
func addStats(tx *gorm.DB, rows []model.StatsMinutely) error {
	return pgdb.Insert(tx, rows, clause.OnConflict{
		Columns: []clause.Column{{Name: "minute"}, {Name: "group_id"}, {Name: "model"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "requests"}, Value: gorm.Expr(model.Prefix + "stats_minutely.requests + EXCLUDED.requests")},
			{Column: clause.Column{Name: "blocked"}, Value: gorm.Expr(model.Prefix + "stats_minutely.blocked + EXCLUDED.blocked")},
		},
	})
}

// RunJob implements pluginsdk.JobRunner.
func (p *Plugin) RunJob(ctx context.Context, in *pluginv1.RunJobRequest) (*pluginv1.RunJobResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	now := p.now().UTC()
	switch in.GetJobId() {
	case JobRollup:
		// Recompute the hourly rows of the last day from the minutely rows
		// (idempotent; covers late events and the current partial hour).
		hours := db.Model(&model.StatsMinutely{}).
			Select("date_trunc('hour', minute, 'UTC'), group_id, model, sum(requests), sum(blocked)").
			Where("minute >= date_trunc('hour', ?::timestamptz, 'UTC')", now.Add(-rollupLookback)).
			Group("1, 2, 3")
		res := pgdb.InsertSelect(db, model.Prefix+"stats_hourly", []string{"hour", "group_id", "model", "requests", "blocked"}, hours,
			&clause.OnConflict{Columns: []clause.Column{{Name: "hour"}, {Name: "group_id"}, {Name: "model"}},
				DoUpdates: clause.AssignmentColumns([]string{"requests", "blocked"})})
		if res.Error != nil {
			return nil, status.Errorf(codes.Internal, "rollup: %v", res.Error)
		}
		return &pluginv1.RunJobResponse{Message: fmt.Sprintf("rolled up %d hourly rows", res.RowsAffected)}, nil
	case JobCleanup:
		var total int64
		for _, q := range []struct {
			model  any
			column string
			before time.Time
		}{
			{&model.StatsMinutely{}, "minute", now.Add(-minutelyRetention)},
			{&model.StatsHourly{}, "hour", now.Add(-longRetention)},
			{&model.BlockLog{}, "occurred_at", now.Add(-longRetention)},
			{&model.ProcessedEvent{}, "processed_at", now.Add(-longRetention)},
		} {
			res := db.Where(clause.Lt{Column: clause.Column{Name: q.column}, Value: q.before}).Delete(q.model)
			if res.Error != nil {
				return nil, status.Errorf(codes.Internal, "cleanup: %v", res.Error)
			}
			total += res.RowsAffected
		}
		return &pluginv1.RunJobResponse{Message: fmt.Sprintf("deleted %d rows", total)}, nil
	default:
		return nil, status.Errorf(codes.NotFound, "unknown job %q", in.GetJobId())
	}
}
