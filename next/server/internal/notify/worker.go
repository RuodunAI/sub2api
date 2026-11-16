package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Start runs the delivery loop until ctx ends or Stop. Every node may run it:
// rows are claimed with SKIP LOCKED and held for the lease.
//
// The end of ctx (the process got SIGTERM) or Stop only ends claiming: the
// delivery in progress runs on a context of its own and finishes within its
// deadline (30 s, grpcruntime.TimeoutNotification). Cancelling it would give
// the attempt back although the provider may already have sent it (an SMTP
// server that accepted DATA before the answer reached the core), and the row
// would be sent again after the restart. Only a drain past DrainTimeout
// cancels it.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	var claim, deliver context.Context
	claim, s.cancel = context.WithCancel(ctx)
	deliver, s.abort = context.WithCancel(context.WithoutCancel(ctx))
	s.done = make(chan struct{})
	go s.loop(claim, deliver, s.done)
}

// abortWait bounds the wait for a delivery cut off by Stop to record its
// outcome (one UPDATE).
const abortWait = 5 * time.Second

// Stop ends claiming, lets the delivery in progress finish for up to
// DrainTimeout (or until ctx ends, the caller's shutdown budget), and only
// then cancels it.
func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	cancel, abort, done := s.cancel, s.abort, s.done
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	defer abort()
	cancel()
	drain := time.NewTimer(s.opts.DrainTimeout)
	defer drain.Stop()
	select {
	case <-done:
		return
	case <-drain.C:
	case <-ctx.Done():
	}
	abort()
	wait := time.NewTimer(abortWait)
	defer wait.Stop()
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
}

// loop claims rows while claim lasts and delivers them on deliver.
func (s *Service) loop(claim, deliver context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(s.opts.PollInterval)
	defer tick.Stop()
	for {
		if s.opts.CanRun == nil || s.opts.CanRun() {
			for claim.Err() == nil && s.step(deliver) {
			}
		}
		select {
		case <-claim.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
	}
}

type queued struct {
	id         int64
	source     string
	channel    string
	provider   *string // the plugin that provided channel at queue time; nil on rows from before 0069
	recipients []string
	subject    string
	body       string
	severity   string
	attempt    int32
}

// step claims and delivers one row; it reports whether there was one. One
// row at a time: a row is delivered well within its lease, so a slow
// provider never lets the lease of a waiting row run out and another node
// send it again.
func (s *Service) step(ctx context.Context) bool {
	var row model.Notification
	res := s.claim(store.G(ctx, s.db.Gorm), &row)
	if res.Error != nil || res.RowsAffected == 0 {
		if res.Error != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "notify: claim", "err", res.Error)
		}
		return false
	}
	q := queued{id: row.ID, source: row.Source, channel: row.Channel, provider: row.Provider, recipients: row.Recipients,
		subject: row.Subject, body: row.Body, severity: core.ValueOr(row.Severity, ""), attempt: int32(row.Attempts)}
	s.finish(ctx, q, s.deliver(ctx, q))
	return true
}

// claimedColumns are the columns a delivery needs, read back from the claim.
var claimedColumns = []clause.Column{{Name: "id"}, {Name: "source"}, {Name: "channel"}, {Name: "provider"}, {Name: "recipients"},
	{Name: "subject"}, {Name: "body"}, {Name: "severity"}, {Name: "attempts"}}

// claim leases the next due row into dst, if any (RowsAffected tells): it
// counts an attempt and holds the row for Lease. SKIP LOCKED lets every
// node claim at once without waiting on each other's rows.
func (s *Service) claim(db *gorm.DB, dst *model.Notification) *gorm.DB {
	next := db.Model(&model.Notification{}).Select("id").
		Where("status IN ('pending', 'sending') AND next_attempt_at <= now() AND attempts < ?", s.opts.MaxAttempts).
		Order("next_attempt_at, id").Limit(1).
		Clauses(store.ForUpdate().SkipLocked())
	return db.Model(dst).Clauses(clause.Returning{Columns: claimedColumns}).Where("id = (?)", next).Updates(map[string]any{
		"status":          "sending",
		"attempts":        gorm.Expr("attempts + 1"),
		"next_attempt_at": gorm.Expr("now() + make_interval(secs => ?)", s.opts.Lease.Seconds()),
	})
}

// errRejected marks a final refusal by the provider.
type errRejected struct{ reason string }

func (e errRejected) Error() string { return e.reason }

// errUnbound: the plugin that provided the channel is not bound to it now
// (not loaded yet at startup, disabled, uninstalled, or the channel went to
// another plugin). Not a delivery attempt: the row waits for it until
// UnboundFor.
var errUnbound = errors.New("the plugin providing the notification channel is not available (it is starting, restarting, disabled or uninstalled)")

// providerNotRunning reports an error of a provider call the plugin instance
// refused before it reached the plugin process: it is starting, waiting to
// restart after a crash, or draining for an upgrade or a stop (grpcruntime
// marks these with a PluginNotRunning method). Like errUnbound, not a
// delivery attempt.
func providerNotRunning(err error) bool {
	var m interface{ PluginNotRunning() bool }
	return errors.As(err, &m) && m.PluginNotRunning()
}

func (s *Service) deliver(ctx context.Context, q queued) error {
	b, ok := s.channel(q.channel)
	if !ok || q.provider != nil && *q.provider != b.Plugin.Key {
		return errUnbound
	}
	resp, err := b.Client.DeliverNotification(ctx, &pluginv1.DeliverNotificationRequest{Id: q.id, Channel: q.channel, Recipients: q.recipients,
		Subject: q.subject, Body: q.body, Severity: q.severity, Source: q.source, Attempt: q.attempt})
	if err != nil {
		return err
	}
	if r := resp.GetRejected(); r != "" {
		return errRejected{r}
	}
	return nil
}

// finish records the outcome unless the lease ran out and another worker
// claimed the row again (attempts moved on). ctx is the delivery context:
// it ends only when Stop gave up waiting for the delivery.
func (s *Service) finish(ctx context.Context, q queued, err error) {
	stopping := ctx.Err() != nil
	ctx = context.WithoutCancel(ctx)
	if exec := s.record(store.G(ctx, s.db.Gorm), q, err, stopping).Error; exec != nil {
		slog.WarnContext(ctx, "notify: record outcome", "id", q.id, "err", exec)
	}
}

// record writes the outcome err of delivering q, guarded by the attempt
// count so that a row claimed again after its lease is left alone.
func (s *Service) record(db *gorm.DB, q queued, err error, stopping bool) *gorm.DB {
	leased := db.Model(&model.Notification{}).Where("id = ? AND status = 'sending' AND attempts = ?", q.id, q.attempt)
	var rejected errRejected
	switch {
	case err != nil && stopping:
		// Cut off by shutdown: not the provider's failure, so the attempt
		// is given back.
		return leased.Updates(map[string]any{"status": "pending", "attempts": gorm.Expr("attempts - 1"), "next_attempt_at": gorm.Expr("now()")})
	case err == nil:
		return leased.Updates(map[string]any{"status": "sent", "sent_at": gorm.Expr("now()"), "last_error": ""})
	case errors.Is(err, errUnbound) || providerNotRunning(err):
		// The attempt is given back and the row is tried again after
		// BackoffMin; it fails only once it waited UnboundFor since it was
		// queued, so a plugin that never comes back does not keep it forever.
		return leased.Updates(map[string]any{
			"attempts":        gorm.Expr("attempts - 1"),
			"last_error":      truncate(err.Error()),
			"status":          gorm.Expr("CASE WHEN created_at < now() - make_interval(secs => ?) THEN 'failed' ELSE 'pending' END", s.opts.UnboundFor.Seconds()),
			"next_attempt_at": gorm.Expr("now() + make_interval(secs => ?)", s.opts.BackoffMin.Seconds()),
		})
	case errors.As(err, &rejected) || int(q.attempt) >= s.opts.MaxAttempts:
		return leased.Updates(map[string]any{"status": "failed", "last_error": truncate(err.Error())})
	default:
		return leased.Updates(map[string]any{
			"status":          "pending",
			"last_error":      truncate(err.Error()),
			"next_attempt_at": gorm.Expr("now() + make_interval(secs => ?)", s.backoff(int(q.attempt)).Seconds()),
		})
	}
}

// backoff doubles from BackoffMin per attempt up to BackoffMax.
func (s *Service) backoff(attempt int) time.Duration {
	d := s.opts.BackoffMin
	for i := 1; i < attempt && d < s.opts.BackoffMax; i++ {
		d *= 2
	}
	return min(d, s.opts.BackoffMax)
}

// PurgeTask is the cluster task that runs purge.
const PurgeTask = "core.notify.purge"

// PurgeTaskSpec runs purge hourly on one node.
func (s *Service) PurgeTaskSpec() core.TaskSpec {
	return core.TaskSpec{
		Name:  PurgeTask,
		Title: core.LocalizedText{"en": "Purge old notifications", "zh": "清理旧通知"},
		Description: core.LocalizedText{
			"en": "Fails notifications whose last delivery attempt never finished and deletes sent or failed ones past their retention.",
			"zh": "把最后一次投递没有完成的通知标为失败，并删除超过保留期的已发送或失败通知。",
		},
		Scope:   core.TaskCluster,
		Every:   time.Hour,
		Timeout: 5 * time.Minute,
		Handler: func(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
			n, err := s.purge(ctx)
			return core.TaskResult{Count: int(n)}, err
		},
	}
}

// purge fails the rows whose last attempt never finished (the lease ran out:
// its node died mid-delivery), which step no longer claims, and deletes the
// finished rows past the retention; it returns how many it deleted.
func (s *Service) purge(ctx context.Context) (int64, error) {
	db := store.G(ctx, s.db.Gorm)
	if err := db.Model(&model.Notification{}).Where("status = 'sending' AND attempts >= ? AND next_attempt_at <= now()", s.opts.MaxAttempts).
		Updates(map[string]any{"status": "failed", "last_error": "the last attempt did not finish"}).Error; err != nil {
		return 0, fmt.Errorf("fail expired rows: %w", err)
	}
	res := db.Where("status IN ('sent', 'failed') AND created_at < now() - make_interval(secs => ?)", s.opts.Retention.Seconds()).
		Delete(&model.Notification{})
	if res.Error != nil {
		return 0, fmt.Errorf("delete old rows: %w", res.Error)
	}
	return res.RowsAffected, nil
}

func truncate(s string) string {
	const max = 1000
	if len(s) <= max {
		return s
	}
	return s[:max]
}
