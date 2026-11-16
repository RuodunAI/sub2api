package job

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/task"
)

// Runtime tasks: the plugin's own cluster tasks besides its manifest jobs,
// registered through HostService (TASK-FRAMEWORK-DESIGN §11.2, CONTRACTS
// §77.8). They are scheduled_tasks rows with source plugin_runtime, which
// every node registers like the manifest jobs' rows.

// Limits of runtime tasks; going past them answers RESOURCE_EXHAUSTED or
// INVALID_ARGUMENT, nothing is dropped silently.
const (
	MaxRuntimeTasks    = 200
	MinRuntimeEvery    = 10 * time.Second
	MaxRuntimeTimeout  = time.Hour
	MaxTaskArgsBytes   = 64 << 10
	MaxQueuedRuns      = 10000
	MaxUniqueKeyLength = 128
)

var taskIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// runtimeRow is a runtime task as stored.
type runtimeRow struct {
	schedule  string
	timeoutMS int64
	attempts  int
	args      string
	enabled   bool
}

// activeVersion reports whether version is the plugin's active version; an
// unknown plugin is not_found.
func (b *Bridge) activeVersion(ctx context.Context, q *gorm.DB, key, version string) (bool, error) {
	p, err := readPlugin(ctx, q, key)
	if store.IsNoRows(err) {
		return false, core.ErrNotFound.WithMessage("plugin not found")
	}
	return err == nil && p.active == version, err
}

// manifestJobIDs are the job ids of the plugin's active manifest: runtime
// tasks may not take them.
func (b *Bridge) manifestJobIDs(ctx context.Context, key, version string) map[string]bool {
	out := map[string]bool{}
	b.reconcileMu.Lock()
	m := b.manifestFor(ctx, b.registry.Current(), key, version)
	b.reconcileMu.Unlock()
	if m != nil {
		for _, j := range m.Jobs {
			out[j.ID] = true
		}
	}
	return out
}

// checkRuntimeTask validates t and returns its row.
func (b *Bridge) checkRuntimeTask(key string, t core.PluginTask, manifestIDs map[string]bool) (runtimeRow, error) {
	invalid := func(format string, args ...any) (runtimeRow, error) {
		return runtimeRow{}, core.ErrInvalidArgument.WithMessage(fmt.Sprintf("task %q: ", t.ID) + fmt.Sprintf(format, args...))
	}
	if !taskIDRe.MatchString(t.ID) {
		return invalid("id must match %s", taskIDRe)
	}
	if manifestIDs[t.ID] {
		return invalid("the id is a manifest job of the plugin")
	}
	var every time.Duration
	var cron string
	if t.Schedule != "" {
		var err error
		if every, cron, err = splitSchedule(t.Schedule); err != nil {
			return invalid("invalid schedule %q: %v", t.Schedule, err)
		}
		if every > 0 && every < MinRuntimeEvery {
			return invalid("runs at most every %s", MinRuntimeEvery)
		}
	}
	timeout := b.opts.DefaultTimeout
	if t.TimeoutSec < 0 || time.Duration(t.TimeoutSec)*time.Second > MaxRuntimeTimeout {
		return invalid("timeout_sec must be between 0 and %d", int(MaxRuntimeTimeout.Seconds()))
	}
	if t.TimeoutSec > 0 {
		timeout = time.Duration(t.TimeoutSec) * time.Second
	}
	args, err := checkArgs(t.Args)
	if err != nil {
		return invalid("%v", err)
	}
	desc, err := task.Describe(core.TaskSpec{Name: TaskName(key, t.ID), Scope: core.TaskCluster, Every: every, Cron: cron,
		Timeout: timeout, Handler: func(context.Context, core.TaskRun) (core.TaskResult, error) { return core.TaskResult{}, nil }},
		task.SourcePluginRuntime, key)
	if err != nil {
		return invalid("%v", err)
	}
	return runtimeRow{schedule: desc.ScheduleText, timeoutMS: timeout.Milliseconds(), attempts: desc.Attempts(), args: args}, nil
}

// checkArgs validates task args: empty, or JSON of at most MaxTaskArgsBytes.
func checkArgs(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if len(raw) > MaxTaskArgsBytes {
		return "", fmt.Errorf("args are %d bytes, at most %d", len(raw), MaxTaskArgsBytes)
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("args must be JSON")
	}
	return string(raw), nil
}

// SyncPluginTasks implements core.PluginTasks: the runtime tasks of the
// plugin become exactly tasks.
func (b *Bridge) SyncPluginTasks(ctx context.Context, key, version string, tasks []core.PluginTask) (bool, error) {
	if len(tasks) > MaxRuntimeTasks {
		return false, core.ErrRateLimited.WithMessage(fmt.Sprintf("at most %d runtime tasks", MaxRuntimeTasks))
	}
	ids := b.manifestJobIDs(ctx, key, version)
	want := map[string]runtimeRow{}
	for _, t := range tasks {
		if _, dup := want[TaskName(key, t.ID)]; dup {
			return false, core.ErrInvalidArgument.WithMessage(fmt.Sprintf("task %q is listed twice", t.ID))
		}
		r, err := b.checkRuntimeTask(key, t, ids)
		if err != nil {
			return false, err
		}
		want[TaskName(key, t.ID)] = r
	}
	return b.writeRuntime(ctx, key, version, func(have map[string]runtimeRow) (map[string]runtimeRow, []string, error) {
		var drop []string
		for name := range have {
			if _, ok := want[name]; !ok {
				drop = append(drop, name)
			}
		}
		return want, drop, nil
	})
}

// UpsertPluginTask implements core.PluginTasks.
func (b *Bridge) UpsertPluginTask(ctx context.Context, key, version string, t core.PluginTask) (bool, error) {
	r, err := b.checkRuntimeTask(key, t, b.manifestJobIDs(ctx, key, version))
	if err != nil {
		return false, err
	}
	name := TaskName(key, t.ID)
	return b.writeRuntime(ctx, key, version, func(have map[string]runtimeRow) (map[string]runtimeRow, []string, error) {
		if _, ok := have[name]; !ok && len(have) >= MaxRuntimeTasks {
			return nil, nil, core.ErrRateLimited.WithMessage(fmt.Sprintf("at most %d runtime tasks", MaxRuntimeTasks))
		}
		return map[string]runtimeRow{name: r}, nil, nil
	})
}

// DeletePluginTask implements core.PluginTasks.
func (b *Bridge) DeletePluginTask(ctx context.Context, key, version, id string) (ignored, deleted bool, err error) {
	name := TaskName(key, id)
	ignored, err = b.writeRuntime(ctx, key, version, func(have map[string]runtimeRow) (map[string]runtimeRow, []string, error) {
		if _, ok := have[name]; ok {
			deleted = true
			return nil, []string{name}, nil
		}
		return nil, nil, nil
	})
	return ignored, deleted && !ignored, err
}

// writeRuntime changes the runtime rows of a plugin in one transaction:
// plan gets the current rows and returns the rows to write and the names to
// delete. Unchanged rows are not written. On a change this node registers
// the tasks at once (so an EnqueueTask right after finds them) and tells the
// other nodes.
func (b *Bridge) writeRuntime(ctx context.Context, key, version string,
	plan func(have map[string]runtimeRow) (map[string]runtimeRow, []string, error)) (bool, error) {
	changed := false
	ignored := false
	err := b.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		// Two nodes syncing the same plugin at once take turns.
		if err := store.XactLockHashtext(ctx, tx, "plugin_runtime_tasks:"+key); err != nil {
			return err
		}
		active, err := b.activeVersion(ctx, tx, key, version)
		if err != nil {
			return err
		}
		if !active {
			ignored = true
			return nil
		}
		have, err := readRuntime(ctx, tx, key)
		if err != nil {
			return err
		}
		put, drop, err := plan(have)
		if err != nil {
			return err
		}
		for _, name := range drop {
			if err := deleteTaskRow(ctx, tx, name, task.SourcePluginRuntime); err != nil {
				return err
			}
			changed = true
		}
		for name, r := range put {
			if old, ok := have[name]; ok && old.schedule == r.schedule && old.timeoutMS == r.timeoutMS && old.attempts == r.attempts && jsonEqual(old.args, r.args) {
				continue
			}
			res := task.UpsertRow(g, map[string]any{"name": name, "source": string(task.SourcePluginRuntime), "plugin_key": key,
				"schedule": r.schedule, "timeout_ms": r.timeoutMS, "max_attempts": r.attempts,
				"args": gorm.Expr("NULLIF(?, '')::jsonb", r.args), "seen_version": b.opts.Version},
				[]string{"schedule", "timeout_ms", "max_attempts", "args", "seen_version"},
				clause.Expr{SQL: "scheduled_tasks.source = EXCLUDED.source AND scheduled_tasks.plugin_key = EXCLUDED.plugin_key"})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return core.ErrConflict.WithMessage(fmt.Sprintf("task %s is taken by a manifest job", name))
			}
			changed = true
		}
		return nil
	})
	if err != nil || ignored {
		return ignored, err
	}
	if changed {
		if err := b.reconcile(ctx); err != nil {
			b.log.Warn("jobs: register runtime tasks", "plugin", key, "err", err)
		}
		if b.bus != nil {
			if err := b.bus.Publish(ctx, core.ChannelTaskChanged, []byte(`{}`)); err != nil {
				b.log.Debug("jobs: announce task change", "err", err)
			}
		}
	}
	return false, nil
}

func readRuntime(ctx context.Context, q *gorm.DB, key string) (map[string]runtimeRow, error) {
	var rows []model.ScheduledTask
	if err := store.G(ctx, q).Select("name", "schedule", "timeout_ms", "max_attempts", "args", "enabled").
		Where("source = ? AND plugin_key = ?", string(task.SourcePluginRuntime), key).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]runtimeRow, len(rows))
	for _, r := range rows {
		// args arrive in jsonb's text form, as args::text gave them; NULL is "".
		out[r.Name] = runtimeRow{schedule: r.Schedule, timeoutMS: int64(r.TimeoutMs), attempts: r.MaxAttempts,
			args: string(r.Args), enabled: core.ValueOr(r.Enabled, false)}
	}
	return out, nil
}

// ListPluginTasks implements core.PluginTasks.
func (b *Bridge) ListPluginTasks(ctx context.Context, key string) ([]core.PluginTask, error) {
	have, err := readRuntime(ctx, b.db.Gorm, key)
	if err != nil {
		return nil, err
	}
	out := make([]core.PluginTask, 0, len(have))
	prefix := "plugin." + key + "."
	for name, r := range have {
		t := core.PluginTask{ID: name[len(prefix):], Schedule: r.schedule, TimeoutSec: int(r.timeoutMS / 1000), Enabled: r.enabled}
		if r.args != "" {
			t.Args = json.RawMessage(r.args)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// EnqueuePluginTask implements core.PluginTasks: one run of a runtime task or
// a cluster manifest job of the plugin.
func (b *Bridge) EnqueuePluginTask(ctx context.Context, key, id string, args json.RawMessage, opts core.EnqueueOpts) error {
	if _, err := checkArgs(args); err != nil {
		return core.ErrInvalidArgument.WithMessage(err.Error())
	}
	if len(opts.UniqueKey) > MaxUniqueKeyLength {
		return core.ErrInvalidArgument.WithMessage(fmt.Sprintf("unique_key is at most %d bytes", MaxUniqueKeyLength))
	}
	name := TaskName(key, id)
	b.mu.Lock()
	tasks, started := b.tasks, b.started && !b.stopped
	reg, ok := b.registered[name]
	var names []string
	for n := range b.registered {
		if strings.HasPrefix(n, "plugin."+key+".") {
			names = append(names, n)
		}
	}
	b.mu.Unlock()
	if !started {
		return core.ErrUnavailable.WithMessage("scheduled tasks are not running on this node")
	}
	if !ok || reg.scope != core.TaskCluster {
		return core.ErrNotFound.WithMessage(fmt.Sprintf("no enabled cluster task %q", id))
	}
	// river_job is River's table, read here with plain SQL; only River's own
	// client writes it.
	var queued int64
	if err := store.G(ctx, b.db.Gorm).Table("river_job").
		Where("kind = 'sup2api.task' AND args->>'name' = ANY(?) AND state IN ('available', 'scheduled', 'pending', 'retryable')", store.Strings(names)).
		Count(&queued).Error; err != nil {
		return err
	}
	if queued >= MaxQueuedRuns {
		return core.ErrRateLimited.WithMessage(fmt.Sprintf("the plugin has %d runs waiting, at most %d", queued, MaxQueuedRuns))
	}
	var payload any
	if len(args) > 0 {
		payload = args
	}
	if err := tasks.Enqueue(ctx, name, payload, opts); err != nil {
		if err == core.ErrTaskUnknown {
			return core.ErrNotFound.WithMessage(fmt.Sprintf("no enabled cluster task %q", id))
		}
		return err
	}
	return nil
}

// jsonEqual compares two JSON texts by value (jsonb reorders keys and
// spacing); "" is no args.
func jsonEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return a == b
	}
	return reflect.DeepEqual(va, vb)
}
