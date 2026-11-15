// Package pluginsdk is the Go SDK for sub2api-next gRPC process plugins.
//
// A plugin is a normal Go binary whose main calls Serve with a value that
// implements one or more capability interfaces:
//
//	//go:embed manifest.json
//	var manifestJSON []byte
//
//	func main() {
//		pluginsdk.Serve(&myPlugin{}, pluginsdk.WithManifest(manifestJSON))
//	}
//
// Capability interfaces mirror the generated pluginv1.XxxServiceServer
// interfaces (without the mustEmbed method):
//
//	Platform   -> PlatformService   ("platform.adapter.v1")
//	Executor   -> PlatformService.Execute ("platform.execute.v1")
//	TaskMonitor -> PlatformService.Monitor ("platform.monitor.v1")
//	Poller     -> PlatformService.Poll ("platform.poll.v1")
//	Hook       -> HookService       ("gateway.hook.v1")
//	JobRunner  -> AppService.RunJob ("app.jobs.v1", see JobMux)
//	EventHandler -> AppService.OnEvents ("app.events.v1")
//	BroadcastHandler -> AppService.OnBroadcast ("app.broadcast.v1", see BroadcastMux)
//	HTTP       -> HTTPService       ("http.routes.v1")
//	AccountRanker -> SchedulerService.RankAccounts ("scheduler.rank.v1")
//	NotificationChannel -> NotificationService.DeliverNotification ("notify.channel.v1")
//	Migration  -> MigrationService  ("migration.data.v1")
//
// The SDK implements PluginService itself. Optional lifecycle interfaces
// (Initializer, Configurer, HealthChecker, Shutdowner) let the plugin hook
// into InitHost, Configure, Health and Shutdown. After InitHost the plugin
// reaches the host through the Host interface (log, KV, database, authz,
// ledger, cluster broadcasts via Publish, cluster locks via Locks).
//
// # Running on several nodes
//
// Every node of a cluster runs its own process of the plugin; write the
// plugin for that from the start (CONTRACTS §27.4):
//
//   - Init runs once per process, i.e. on every node, and so does any
//     goroutine it starts. Declare periodic work as manifest jobs[]: each
//     trigger runs on one node for the whole cluster.
//   - Request execution uses Executor and ExecuteDefault: build, forward
//     through the host, then RecordUsage or ReserveAndWatch before returning.
//     Prices, attribution and all ledger writes remain host-owned.
//   - Upstream async work uses endpoint.task + TaskSubmissionParser +
//     TaskMonitor. The host records owner and original account before success,
//     coordinates polling and serves shared snapshots on every node. Do not
//     poll the same task from jobs[], Init or client query handlers.
//     In Monitor, call ExecuteHTTP and ReportTaskProgress with its context;
//     the host atomically records progress and settlement, and binds the request
//     to the account proxy and limits. Generic Egress is a separate permission
//     and does not inherit the polling execution's account limits.
//   - Host.Locks gives cross-node mutual exclusion for other work. The locks
//     are not fenced, so the guarded work must still be idempotent.
//   - KV has no compare-and-set: two nodes doing read-modify-write on one key
//     can lose an update. Use the database (a transaction) or a lock.
//   - Publish is best effort and the publishing node does not receive its
//     own message: apply a change locally, then publish it.
//   - OnEvents is delivered at least once (idempotent on Event.Id), and
//     MigrateData runs once per rollout (the host records it), but a
//     record lost after a successful call means it runs again; both must
//     be idempotent (CONTRACTS §11.7, §36). So must migrations/*.sql.
//
// # Database
//
// A plugin with a database (manifest "database", grant "db.schema") owns one
// schema, plg_<key>. Host.DB returns a GORM handle on it (package pgdb:
// search_path on the schema, the egress tunnel in strict network mode).
// Plugin code does not import pgx. Errors wrap the driver's. Those of Exec,
// Create, Save, Find, Scan and the like are translated too: a unique
// violation also matches gorm.ErrDuplicatedKey (foreign key and check
// violations gorm.ErrForeignKeyViolated and gorm.ErrCheckConstraintViolated),
// and First/Take report a missing row as gorm.ErrRecordNotFound. Those of
// Raw(...).Row() and Rows() are not: test for a duplicate key with
// pgdb.IsUniqueViolation, which holds on either path.
//
// The tables are described twice, and the two must agree:
//
//   - The GORM models of the plugin's internal/model package are the one
//     description to edit. The package exports every model:
//
//     func All() []any { return []any{&Order{}, &Item{}} }
//
//     Each model names its table (TableName) and spells every column out
//     as it is in the database: `gorm:"type:jsonb;not null;default:'{}'"`,
//     a PostgreSQL array as a pgdb.Array[T] with its type (type:text[]),
//     an index, check or foreign key constraint by its name
//     (index:<name>, check:<name>,<expr>, constraint:<name>,). A unique
//     column is `unique`: GORM names a constraint it creates
//     uni_<table>_<column>, and takes any unique constraint on just that
//     column for it. Name a new one so in the migration too, or declare
//     uniqueIndex:<name>: a model that drops `unique` from a column whose
//     constraint has another name cannot be diffed (check lists the
//     constraint; drop it by hand). Turn GORM's automatic
//     created_at/updated_at off (autoCreateTime:false, autoUpdateTime:false)
//     where the database default fills them.
//
//   - migrations/*.sql are what the host runs: in file name order, each in
//     its own transaction with search_path on the schema, and idempotent.
//     The plugin never runs AutoMigrate.
//
// The sub2api-plugin command keeps the two together:
//
//	sub2api-plugin db diff --dir <plugin> [--name add_score]  # writes migrations/NNNN_add_score.sql
//	sub2api-plugin db check --dir <plugin>                    # fails while they disagree; pack runs it
//
// Both run migrations/ on a scratch schema of the test PostgreSQL
// (TEST_DATABASE_URL, or the local server of package testpg), run GORM's
// AutoMigrate for All() on it until it changes nothing and look at the DDL it
// ran (pgdb.Drift). Column defaults are compared by what PostgreSQL stores
// for them (pg_get_expr), not as text: '{}' and '{}'::jsonb, ARRAY['a'] and
// ARRAY['a'::text] are the same. To reach internal/model they generate a
// main package in the plugin module and go run it (pgdb.DriftMain). diff
// writes that DDL, made idempotent, as the next migration, numbered with
// as many digits as the existing ones (the host runs them in file name
// order), after running it twice on a scratch schema and finding nothing
// left to change; or says nothing is to do. It writes nothing when a change
// can lose data or fail on the rows of a live table (a column type, a drop,
// SET NOT NULL, a NOT NULL column without a default, a CHECK, UNIQUE or
// foreign key on a table that already exists: marked "!") or the
// schema has what no model maps - a table, column, index, check, foreign
// key or unique constraint (a field removed from the code), a view,
// sequence or function: such a migration is written by hand. check fails on
// any difference; pack runs it with the overlay's migrations. AutoMigrate
// knows indexes and constraints by name only: change one under a new name.
//
// The test PostgreSQL is TEST_DATABASE_URL when set. Otherwise diff, check
// and pack start the local server of package testpg (downloaded once) and
// stop it again when they started it.
//
// Tests run the real migrations: pluginsdktest.NewSchema and ApplyMigrations.
//
// The pluginsdktest sub-package runs a plugin in-process over bufconn with a
// fake host, for unit tests.
package pluginsdk
