package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivertype"
	"golang.org/x/sync/semaphore"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/background"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// River settings (TASK-FRAMEWORK-DESIGN §7).
const (
	taskKind          = "sup2api.task"
	queueCore         = "core"
	queueCoreWorkers  = 4
	rescueStuckAfter  = 75 * time.Minute
	completedRetain   = 24 * time.Hour
	failedRetain      = 7 * 24 * time.Hour
	riverSoftStop     = 20 * time.Second
	maxArgsBytes      = 64 << 10
	manualKey         = "manual"
	continueKey       = "continue:"
	leaseGrace        = time.Minute
	executorGroupSize = 4
	executorQueue     = 16
	// fetchPoll is how often a node polls for runs (River's default; snooze
	// spreads put-offs over it). River runs poll-only on database/sql (no
	// LISTEN). A run inserted outside a transaction (Enqueue without one,
	// Trigger, a continuation) on a node whose River client is started wakes
	// that node's producer and is fetched at once; every other run - inserted
	// in a transaction (EnqueueTx: InsertTx wakes nothing), on another node,
	// or on a node that has not started River - waits up to fetchPoll for a
	// node that runs River to poll.
	fetchPoll = time.Second
)

// queuePlugin runs the plugin tasks, so slow plugin tasks do not hold up
// core tasks.
const (
	queuePlugin        = "plugin"
	queuePluginWorkers = 4
)

// Variables so tests can shorten them.
var (
	// admissionPoll is how often the cluster backend reads CanWork.
	admissionPoll = time.Second
	// unknownGrace is how long a run of a task this node does not know is
	// snoozed (another node may know it) before it is cancelled.
	unknownGrace = 10 * time.Minute
	// snoozeBusy / snoozeElsewhere delay a run whose lease is taken / that
	// this node cannot run now.
	snoozeBusy      = 10 * time.Second
	snoozeElsewhere = 5 * time.Second
	// retryDelay is the wait before attempt+1 of a failed run.
	retryDelay = defaultRetryDelay
)

// defaultRetryDelay waits attempt⁴ seconds, at most half the task's period
// and at most 10 minutes.
func defaultRetryDelay(attempt int, period time.Duration) time.Duration {
	d := time.Duration(attempt*attempt*attempt*attempt) * time.Second
	if period > 0 && d > period/2 {
		d = period / 2
	}
	return min(d, 10*time.Minute)
}

// unfinishedStates are the River states a unique key covers for manual runs
// and Enqueue with a UniqueKey: finished runs do not block a new one.
var unfinishedStates = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
	rivertype.JobStateRetryable, rivertype.JobStateScheduled,
}

// taskArgs are the arguments of the one River kind every cluster run uses.
// name and key make up the unique key: periodic runs have no key (unique per
// slot), manual runs "manual", enqueued runs their UniqueKey.
type taskArgs struct {
	Name   string          `json:"name" river:"unique"`
	Key    string          `json:"key,omitempty" river:"unique"`
	Args   json.RawMessage `json:"args,omitempty"`
	Manual bool            `json:"manual,omitempty"`
}

func (taskArgs) Kind() string { return taskKind }

// runOutput is recorded on the river_job row of every run (§8.2).
type runOutput struct {
	Status     string `json:"status"`
	Count      int    `json:"count"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	Node       string `json:"node"`
	DurationMS int64  `json:"duration_ms"`
	Manual     bool   `json:"manual,omitempty"`
}

// clusterBackend runs cluster tasks through River (TASK-FRAMEWORK-DESIGN §7).
type clusterBackend struct {
	log      *slog.Logger
	db       *store.DB
	reg      *Registry
	nodeID   string
	clientID string
	version  string
	canWork  func() bool
	// group and pluginGroup admit the runs of core and plugin tasks into
	// the node's background executor, each with its own share.
	group       *background.Group
	pluginGroup *background.Group

	client *river.Client[*sql.Tx]
	// starting keeps an Insert outside a transaction from overlapping
	// client.Start. Without a listener (database/sql), River has such an
	// Insert wake the producer of its queue, which reads state that Start
	// writes without a lock (river v0.49.0, producer.fetchLimiter): Start
	// holds all of it, Insert one share. InsertTx wakes nothing. A
	// semaphore rather than a sync.RWMutex: Start can take a while (each
	// producer's ProducerInit has 10 seconds), and an Insert on a request
	// path waits for it only as long as its context allows.
	starting *semaphore.Weighted

	mu       sync.Mutex
	periodic map[string]bool
	running  bool // the River client is started
	stopped  bool
	stop     chan struct{}
	done     chan struct{}
	runCtx   context.Context
	cancel   context.CancelFunc
}

func newClusterBackend(ctx context.Context, log *slog.Logger, db *store.DB, reg *Registry, nodeID, clientID, version string,
	canWork func() bool, exec *background.Executor) (*clusterBackend, error) {
	b := &clusterBackend{log: log, db: db, reg: reg, nodeID: nodeID, clientID: clientID, version: version, canWork: canWork,
		starting: semaphore.NewWeighted(startShares), periodic: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	b.runCtx, b.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if exec != nil {
		b.group = exec.Group(b.runCtx, executorGroupSize, executorQueue)
		b.pluginGroup = exec.Group(b.runCtx, executorGroupSize, executorQueue)
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &taskWorker{b: b}); err != nil {
		return nil, err
	}
	queues := map[string]river.QueueConfig{queueCore: {MaxWorkers: queueCoreWorkers}, queuePlugin: {MaxWorkers: queuePluginWorkers}}
	client, err := river.NewClient(riverdatabasesql.New(db.SQL), &river.Config{
		ID:                          clientID,
		Logger:                      log.With("subsystem", "river"),
		Workers:                     workers,
		Queues:                      queues,
		MaxAttempts:                 5,
		FetchPollInterval:           fetchPoll,
		PollOnly:                    true, // the database/sql driver has no listener (see fetchPoll)
		RescueStuckJobsAfter:        rescueStuckAfter,
		CompletedJobRetentionPeriod: completedRetain,
		CancelledJobRetentionPeriod: failedRetain,
		DiscardedJobRetentionPeriod: failedRetain,
		SoftStopTimeout:             riverSoftStop,
		ReindexerSchedule:           river.NeverSchedule(),
	})
	if err != nil {
		return nil, err
	}
	b.client = client
	return b, nil
}

// start records the registered tasks, configures their periodic runs and
// starts the admission watcher, which starts River while CanWork holds.
func (b *clusterBackend) start(ctx context.Context) error {
	for _, t := range b.reg.List(core.TaskCluster) {
		if err := b.add(ctx, t); err != nil {
			return err
		}
	}
	go b.watchAdmission()
	return nil
}

// add records a core task in scheduled_tasks (the rows of plugin tasks are
// written by the plugin task bridge, internal/job) and adds its periodic
// runs.
func (b *clusterBackend) add(ctx context.Context, t *Task) error {
	if t.Source == SourceCore {
		if err := b.upsert(ctx, t); err != nil {
			return fmt.Errorf("record task %s: %w", t.Name, err)
		}
	}
	if t.Schedule == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.periodic[t.Name] {
		return nil
	}
	name, attempts, period, queue := t.Name, t.Attempts(), t.Period, t.queue()
	job := river.NewPeriodicJob(t.Schedule, func() (river.JobArgs, *river.InsertOpts) {
		return taskArgs{Name: name}, &river.InsertOpts{Queue: queue, MaxAttempts: attempts,
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: period}}
	}, &river.PeriodicJobOpts{ID: name, RunOnStart: t.RunOnStart})
	if _, err := b.client.PeriodicJobs().AddSafely(job); err != nil {
		return fmt.Errorf("periodic task %s: %w", t.Name, err)
	}
	b.periodic[t.Name] = true
	return nil
}

// remove drops the periodic runs of name. Runs already inserted stay and
// meet an unregistered task (see taskWorker.Work).
func (b *clusterBackend) remove(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.periodic[name] {
		b.client.PeriodicJobs().RemoveByID(name)
		delete(b.periodic, name)
	}
}

func (b *clusterBackend) upsert(ctx context.Context, t *Task) error {
	var pluginKey *string
	if t.PluginKey != "" {
		pluginKey = &t.PluginKey
	}
	// Raw SQL: VALUES and the conflict branch set seen_at, and the branch
	// updated_at, to now(), which a model's time.Time cannot carry (store
	// stamps only created_at and updated_at, and only on INSERT); the
	// clause.OnConflict tree of EXCLUDED columns and expressions would only
	// restate this statement.
	return store.G(ctx, b.db.Gorm).Exec(`INSERT INTO scheduled_tasks (name, source, plugin_key, schedule, timeout_ms, max_attempts, seen_version, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, now())
		ON CONFLICT (name) DO UPDATE SET source = EXCLUDED.source, plugin_key = EXCLUDED.plugin_key, schedule = EXCLUDED.schedule,
			timeout_ms = EXCLUDED.timeout_ms, max_attempts = EXCLUDED.max_attempts, seen_version = EXCLUDED.seen_version,
			seen_at = now(), updated_at = now()`,
		t.Name, string(t.Source), pluginKey, t.ScheduleText, t.Timeout.Milliseconds(), t.Attempts(), b.version).Error
}

// taskRowExists reports whether name has a scheduled_tasks row.
func taskRowExists(ctx context.Context, q *gorm.DB, name string) (bool, error) {
	var n int64
	err := store.G(ctx, q).Model(&model.ScheduledTask{}).Where("name = ?", name).Count(&n).Error
	return n > 0, err
}

// releaseLeases frees the leases of the tasks cond picks and sets
// updated_at to at, now() or clock_timestamp() as the raw statement each
// caller replaced had it.
func releaseLeases(db *gorm.DB, at, cond string, args ...any) *gorm.DB {
	return db.Model(&model.ScheduledTask{}).Where(cond, args...).
		Updates(map[string]any{"lease_job_id": nil, "lease_node": nil, "lease_until": nil, "updated_at": gorm.Expr(at)})
}

// watchAdmission starts River while this node may do background work and
// stops it when it may not (§7.1).
func (b *clusterBackend) watchAdmission() {
	defer close(b.done)
	tick := time.NewTicker(admissionPoll)
	defer tick.Stop()
	for {
		b.converge()
		select {
		case <-b.stop:
			return
		case <-tick.C:
		}
	}
}

func (b *clusterBackend) converge() {
	want := b.canWork()
	b.mu.Lock()
	running, stopped := b.running, b.stopped
	b.mu.Unlock()
	switch {
	case stopped || want == running:
		return
	case want:
		err := b.starting.Acquire(b.runCtx, startShares)
		if err == nil {
			err = b.client.Start(b.runCtx)
			b.starting.Release(startShares)
		}
		if err != nil {
			b.log.Warn("task: start River", "err", err)
			return
		}
		b.log.Info("task: cluster tasks started on this node")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), riverSoftStop+5*time.Second)
		err := b.client.Stop(ctx)
		cancel()
		if err != nil {
			b.log.Warn("task: stop River", "err", err)
		}
		b.log.Info("task: cluster tasks stopped on this node (no background admission)")
	}
	b.mu.Lock()
	b.running = want
	b.mu.Unlock()
}

// enqueue inserts one run of a cluster task (tx nil: outside a transaction).
func (b *clusterBackend) enqueue(ctx context.Context, tx *gorm.DB, name string, args any, opts core.EnqueueOpts) error {
	t, ok := b.reg.Get(name)
	if !ok || t.Scope != core.TaskCluster {
		return core.ErrTaskUnknown
	}
	var raw json.RawMessage
	if args != nil {
		var err error
		if raw, err = json.Marshal(args); err != nil {
			return fmt.Errorf("task %s: encode args: %w", name, err)
		}
		if len(raw) > maxArgsBytes {
			return fmt.Errorf("task %s: args are %d bytes, at most %d", name, len(raw), maxArgsBytes)
		}
	}
	insert := &river.InsertOpts{Queue: t.queue(), MaxAttempts: t.Attempts(), ScheduledAt: opts.RunAt}
	if opts.UniqueKey != "" {
		insert.UniqueOpts = river.UniqueOpts{ByArgs: true, ByState: unfinishedStates}
	}
	job := taskArgs{Name: name, Key: opts.UniqueKey, Args: raw}
	if tx == nil {
		return b.insert(ctx, job, insert)
	}
	sqlTx := store.SQLTx(tx)
	if sqlTx == nil {
		return fmt.Errorf("task %s: enqueue in a handle that is no transaction", name)
	}
	_, err := b.client.InsertTx(ctx, sqlTx, job, insert)
	return err
}

// startShares is the weight of starting that client.Start takes: all of
// it. Each Insert takes one, so this many can run at once.
const startShares = 1 << 30

// insert inserts a run outside a transaction once no client.Start is
// running (see starting), or fails with ctx's error.
func (b *clusterBackend) insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) error {
	if err := b.starting.Acquire(ctx, 1); err != nil {
		return err
	}
	defer b.starting.Release(1)
	_, err := b.client.Insert(ctx, args, opts)
	return err
}

// trigger inserts a manual run; one manual run waits at a time.
func (b *clusterBackend) trigger(ctx context.Context, name string) error {
	t, ok := b.reg.Get(name)
	if !ok || t.Scope != core.TaskCluster {
		return core.ErrTaskUnknown
	}
	return b.insert(ctx, taskArgs{Name: name, Key: manualKey, Manual: true}, &river.InsertOpts{
		Queue: t.queue(), MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: unfinishedStates}})
}

// shutdown stops the admission watcher and River (soft stop, then cancel).
func (b *clusterBackend) shutdown(ctx context.Context) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	running := b.running
	b.mu.Unlock()
	close(b.stop)
	if running {
		stopCtx, cancel := context.WithTimeout(ctx, riverSoftStop+5*time.Second)
		if err := b.client.Stop(stopCtx); err != nil {
			b.log.Warn("task: stop River", "err", err)
		}
		cancel()
	}
	for _, g := range []*background.Group{b.group, b.pluginGroup} {
		if g != nil {
			closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = g.Close(closeCtx)
			cancel()
		}
	}
	b.cancel()
}

// taskWorker works every sup2api.task job.
type taskWorker struct {
	river.WorkerDefaults[taskArgs]
	b *clusterBackend
}

func (w *taskWorker) Timeout(job *river.Job[taskArgs]) time.Duration {
	if t, ok := w.b.reg.Get(job.Args.Name); ok {
		return t.Timeout
	}
	return time.Minute
}

func (w *taskWorker) NextRetry(job *river.Job[taskArgs]) time.Time {
	var period time.Duration
	if t, ok := w.b.reg.Get(job.Args.Name); ok {
		period = t.Period
	}
	return time.Now().Add(retryDelay(job.Attempt, period))
}

func (w *taskWorker) Work(ctx context.Context, job *river.Job[taskArgs]) error {
	b := w.b
	t, ok := b.reg.Get(job.Args.Name)
	if !ok || t.Scope != core.TaskCluster {
		if time.Since(job.CreatedAt) < unknownGrace {
			return snooze(snoozeElsewhere)
		}
		return river.JobCancel(fmt.Errorf("task %s is not registered on any node that picked it up", job.Args.Name))
	}
	group := b.group
	if t.Source != SourceCore {
		group = b.pluginGroup
	}
	if group == nil {
		return b.work(ctx, t, job)
	}
	permit, err := group.Acquire(ctx)
	if err != nil {
		// This node stopped admitting background work: let another one run it.
		return snooze(snoozeElsewhere)
	}
	var out error
	_ = permit.Run(ctx, background.Work{}, func(ctx context.Context) error {
		out = b.work(ctx, t, job)
		return nil
	})
	return out
}

// snooze puts a run off for d plus a random part of a fetch poll. A put-off
// of whole seconds ends just before the next poll of the node that put the
// run off, which then fetches it again ahead of the other nodes: a node that
// cannot run a task (its plugin is not loaded there) would keep the run from
// the nodes that can. The random part makes every node as likely to fetch
// the run next.
func snooze(d time.Duration) error { return river.JobSnooze(d + rand.N(fetchPoll)) }

// work runs one attempt under the task's lease and records its outcome.
func (b *clusterBackend) work(ctx context.Context, t *Task, job *river.Job[taskArgs]) error {
	started := time.Now()
	record := func(o runOutput) {
		o.Node, o.Manual = b.nodeID, job.Args.Manual
		o.DurationMS = time.Since(started).Milliseconds()
		if err := river.RecordOutput(ctx, o); err != nil {
			b.log.Debug("task: record output", "task", t.Name, "err", err)
		}
	}
	leased, err := b.takeLease(ctx, t, job.ID)
	if errors.Is(err, errTaskRemoved) {
		record(runOutput{Status: StatusSkipped, Message: "the task was removed"})
		return nil
	}
	if err != nil {
		return err
	}
	if !leased {
		if job.Args.Manual || job.Args.Key != "" || t.Schedule == nil {
			return snooze(snoozeBusy)
		}
		record(runOutput{Status: StatusSkipped, Message: "the previous run is still going"})
		return nil
	}

	res, herr := callHandler(ctx, t, core.TaskRun{Name: t.Name, ScheduledAt: job.ScheduledAt, Attempt: job.Attempt,
		Args: job.Args.Args, Manual: job.Args.Manual, NodeID: b.nodeID, RunID: strconv.FormatInt(job.ID, 10),
		QueuedAt: job.CreatedAt})
	timedOut := ctx.Err() == context.DeadlineExceeded
	o := runOutput{Count: res.Count, Message: truncate(res.Message)}
	var retry *core.TaskRetryAfterError
	var skip *core.TaskSkippedError
	var cont *core.TaskContinueError
	var ret error
	switch {
	case herr == nil:
		o.Status = StatusSucceeded
	case errors.As(herr, &cont):
		o.Status = StatusContinued
	case errors.As(herr, &retry):
		o.Status, o.Message = StatusSkipped, truncate(herr.Error())
		ret = snooze(retry.Delay)
	case errors.As(herr, &skip):
		o.Status, o.Message = StatusSkipped, truncate(skip.Reason)
	case timedOut:
		o.Status, o.Error, ret = StatusTimeout, truncate(herr.Error()), herr
	default:
		o.Status, o.Error, ret = StatusFailed, truncate(herr.Error()), herr
	}
	if err := b.finish(context.WithoutCancel(ctx), t, job.ID, o, retry != nil); err != nil {
		b.log.Warn("task: record run", "task", t.Name, "err", err)
	}
	if o.Status == StatusFailed || o.Status == StatusTimeout {
		b.log.Warn("task: cluster task failed", "task", t.Name, "status", o.Status, "err", o.Error,
			"attempt", job.Attempt, "max_attempts", job.MaxAttempts)
	}
	if cont != nil {
		b.continueRun(context.WithoutCancel(ctx), t, job, cont.Delay)
	}
	record(o)
	return ret
}

// continueRun inserts the continuation of job: one run after delay with the
// same args. Its key names the run it continues, so a retried insert does
// not make two; the lease keeps it from overlapping a scheduled run. A
// failed insert leaves the work to the next scheduled run.
func (b *clusterBackend) continueRun(ctx context.Context, t *Task, job *river.Job[taskArgs], delay time.Duration) {
	err := b.insert(ctx, taskArgs{Name: t.Name, Key: continueKey + strconv.FormatInt(job.ID, 10), Args: job.Args.Args},
		&river.InsertOpts{Queue: t.queue(), MaxAttempts: t.Attempts(), ScheduledAt: time.Now().Add(delay),
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: unfinishedStates}})
	if err != nil {
		b.log.Warn("task: insert the continuation", "task", t.Name, "run_id", job.ID, "err", err)
	}
}

// errTaskRemoved is takeLease's answer for a plugin task without a row.
var errTaskRemoved = errors.New("task: removed")

// takeLease claims the task's lease for job (§7.4); false when another run
// holds it.
func (b *clusterBackend) takeLease(ctx context.Context, t *Task, jobID int64) (bool, error) {
	// Raw SQL: the lease is a compare-and-set on the row, timed by
	// clock_timestamp() and an interval parameter.
	res := store.G(ctx, b.db.Gorm).Exec(`UPDATE scheduled_tasks
		SET lease_job_id = ?, lease_node = ?, lease_until = clock_timestamp() + ?::interval,
			last_started_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE name = ? AND (lease_until IS NULL OR lease_until < clock_timestamp() OR lease_job_id = ?)`,
		jobID, b.nodeID, (t.Timeout + leaseGrace).String(), t.Name, jobID)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		// A task registered on this node but missing its row: a core task's
		// row was pruned, record it and try once more; a plugin task's row
		// is gone because the task was removed.
		exists, err := taskRowExists(ctx, b.db.Gorm, t.Name)
		if err != nil {
			return false, err
		}
		if !exists && t.Source != SourceCore {
			return false, errTaskRemoved
		}
		if !exists {
			if err := b.upsert(ctx, t); err != nil {
				return false, err
			}
			return b.takeLease(ctx, t, jobID)
		}
		return false, nil
	}
	return true, nil
}

// finish releases the lease of job and, unless the run was only put off,
// records it as the task's latest run.
func (b *clusterBackend) finish(ctx context.Context, t *Task, jobID int64, o runOutput, putOff bool) error {
	g := store.G(ctx, b.db.Gorm)
	if putOff {
		return releaseLeases(g, "clock_timestamp()", "name = ? AND lease_job_id = ?", t.Name, jobID).Error
	}
	// scheduled_tasks.last_status keeps the 0.1.235 values (its CHECK
	// constraint): a continued run is a success there; its run record keeps
	// "continued".
	status := o.Status
	if status == StatusContinued {
		status = StatusSucceeded
	}
	return recordRun(g, t.Name, jobID, b.nodeID, status, o).Error
}

// recordRun makes job the task's latest run, recorded with status (a
// last_status value) and o, and releases the lease if job still holds it.
// Raw SQL: the release is conditional and the counters move by the status,
// all in one statement; the named parameters stand for values the SQL uses
// more than once.
func recordRun(g *gorm.DB, name string, jobID int64, nodeID, status string, o runOutput) *gorm.DB {
	return g.Exec(`UPDATE scheduled_tasks SET
			lease_job_id = CASE WHEN lease_job_id = @job THEN NULL ELSE lease_job_id END,
			lease_node = CASE WHEN lease_job_id = @job THEN NULL ELSE lease_node END,
			lease_until = CASE WHEN lease_job_id = @job THEN NULL ELSE lease_until END,
			last_job_id = @job, last_node = @node, last_finished_at = clock_timestamp(), last_status = @status,
			last_count = @count, last_message = NULLIF(@message, ''), last_error = NULLIF(@error, ''),
			last_success_at = CASE WHEN @status = 'succeeded' THEN clock_timestamp() ELSE last_success_at END,
			consecutive_failures = CASE WHEN @status = 'succeeded' THEN 0 WHEN @status = 'skipped' THEN consecutive_failures
				ELSE consecutive_failures + 1 END,
			updated_at = clock_timestamp()
		WHERE name = @name`, map[string]any{"name": name, "job": jobID, "node": nodeID, "status": status,
		"count": o.Count, "message": o.Message, "error": o.Error})
}
