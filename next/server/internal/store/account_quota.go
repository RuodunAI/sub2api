package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// Account quota snapshots (table account_quota_snapshots, CONTRACTS §44).

// Values of QuotaSnapshot.Source.
const (
	QuotaPassive = "passive"
	QuotaActive  = "active"
)

// QuotaWindow is one stored quota window; the map key of
// QuotaSnapshot.Windows is its window key.
type QuotaWindow struct {
	// Utilization is in percent (0-100, more with overage).
	Utilization        float64    `json:"utilization"`
	UtilizationUnknown bool       `json:"utilization_unknown,omitempty"`
	ResetsAt           *time.Time `json:"resets_at,omitempty"`
	Status             string     `json:"status,omitempty"`
	Used               int64      `json:"used,omitempty"`
	Limit              int64      `json:"limit,omitempty"`
}

// QuotaSnapshot is one account_quota_snapshots row. The JSON form is the one
// the account service caches in Redis (CONTRACTS §51.3).
type QuotaSnapshot struct {
	AccountID int64                  `json:"account_id"`
	Windows   map[string]QuotaWindow `json:"windows"`
	Source    string                 `json:"source"`
	Error     string                 `json:"error"`
	// UpdatedAt is when the windows were last written; nil = no data yet
	// (a row may exist only because an active query was attempted).
	UpdatedAt     *time.Time `json:"updated_at"`
	LastPassiveAt *time.Time `json:"last_passive_at"`
	LastActiveAt  *time.Time `json:"last_active_at"`
	Revision      int64      `json:"revision"`
	Generation    int64      `json:"generation"`
	NextCheckAt   *time.Time `json:"next_check_at,omitempty"`
	RefreshStatus string     `json:"refresh_status"`
}

const selectQuota = `SELECT account_id, windows, source, error, updated_at, last_passive_at, last_active_at, revision, next_check_at, refresh_status, generation
	FROM account_quota_snapshots`

func scanQuota(r RowScanner) (*QuotaSnapshot, error) {
	var q QuotaSnapshot
	var raw []byte
	if err := r.Scan(&q.AccountID, &raw, &q.Source, &q.Error, &q.UpdatedAt, &q.LastPassiveAt, &q.LastActiveAt, &q.Revision, &q.NextCheckAt, &q.RefreshStatus, &q.Generation); err != nil {
		return nil, err
	}
	var err error
	if q.Windows, err = quotaWindows(raw); err != nil {
		return nil, err
	}
	return &q, nil
}

// quotaWindows decodes the windows document of a snapshot row; an empty one
// holds no windows.
func quotaWindows(raw []byte) (map[string]QuotaWindow, error) {
	w := map[string]QuotaWindow{}
	if len(raw) == 0 {
		return w, nil
	}
	return w, json.Unmarshal(raw, &w)
}

// AccountQuota returns the snapshot of one account, nil when it has none.
func (d *DB) AccountQuota(ctx context.Context, accountID int64) (*QuotaSnapshot, error) {
	q, err := scanQuota(G(ctx, d.Gorm).Raw(selectQuota+` WHERE account_id = ?`, accountID).Row())
	if IsNoRows(err) {
		return nil, nil
	}
	return q, err
}

// AccountQuotas returns the snapshots of the given accounts in one query;
// accounts without one are absent from the map.
func (d *DB) AccountQuotas(ctx context.Context, accountIDs []int64) (map[int64]*QuotaSnapshot, error) {
	out := map[int64]*QuotaSnapshot{}
	if len(accountIDs) == 0 {
		return out, nil
	}
	err := EachRow(G(ctx, d.Gorm).Raw(selectQuota+` WHERE account_id = ANY(?)`, Int64s(accountIDs)), func(rows *sql.Rows) error {
		q, err := scanQuota(rows)
		if err == nil {
			out[q.AccountID] = q
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AccountQuotasAt is AccountQuotas plus the time the read began
// (statement_timestamp of the one statement, on the database clock): the
// result reflects at least every write committed before it. Caches order
// their entries by it (CONTRACTS §51.3).
func (d *DB) AccountQuotasAt(ctx context.Context, accountIDs []int64) (map[int64]*QuotaSnapshot, time.Time, error) {
	read, err := readQuotaRows(quotasAt(G(ctx, d.Gorm), accountIDs), false)
	if err != nil {
		return nil, time.Time{}, err
	}
	// Unlike the quota mirror, the callers of this read take no partial
	// result: a malformed row (any one) fails it.
	for _, err := range read.Invalid {
		return nil, read.At, err
	}
	return read.Snapshots, read.At, nil
}

// quotasAt selects the rows of AccountQuotasAt: ReadAccountQuotas's read on
// GORM.
func quotasAt(db *gorm.DB, accountIDs []int64) *gorm.DB {
	return db.Raw(`SELECT NULL::bigint, `+selectQuotaRead+`
		FROM (SELECT statement_timestamp()) t LEFT JOIN account_quota_snapshots q ON q.account_id = ANY(?)`, Int64s(accountIDs))
}

// QuotaRead is a read of account quota snapshots for the core's in-memory
// quota mirror (MODEL-AVAILABILITY-PRECHECK-DESIGN §4). A row that cannot be
// decoded fails only itself.
type QuotaRead struct {
	// At is when the read began (statement_timestamp of the one statement,
	// on the database clock): the read reflects every write committed before.
	At time.Time
	// Accounts lists every account the read covered, with or without a
	// snapshot row; only ReadAccountQuotasOfTypes fills it.
	Accounts []int64
	// Snapshots holds the decoded rows by account id.
	Snapshots map[int64]*QuotaSnapshot
	// Invalid holds the rows whose windows document could not be decoded.
	Invalid map[int64]error
}

const selectQuotaRead = `statement_timestamp(), q.account_id, q.windows, COALESCE(q.source, ''), COALESCE(q.error, ''),
		q.updated_at, q.last_passive_at, q.last_active_at, COALESCE(q.revision,0), q.next_check_at, COALESCE(q.refresh_status,'pending'), COALESCE(q.generation,0)`

// ReadAccountQuotas reads the snapshot rows of accountIDs; accounts without
// one are absent from Snapshots and Invalid. The quota mirror reloads after
// each applied observation, so the rows of the Raw statement (quotasAt) are
// scanned directly (BenchmarkHotReadAccountQuotas).
func (d *DB) ReadAccountQuotas(ctx context.Context, accountIDs []int64) (*QuotaRead, error) {
	return readQuotaRows(quotasAt(G(ctx, d.Gorm), accountIDs), false)
}

// ReadAccountQuotasOfTypes reads the snapshot rows of every account not
// deleted whose "plugin_key/type" is in keys; Accounts lists them all.
func (d *DB) ReadAccountQuotasOfTypes(ctx context.Context, keys []string) (*QuotaRead, error) {
	return readQuotaRows(G(ctx, d.Gorm).Raw(`SELECT a.id, `+selectQuotaRead+`
		FROM (SELECT statement_timestamp()) t LEFT JOIN (accounts a LEFT JOIN account_quota_snapshots q ON q.account_id = a.id)
			ON a.deleted_at IS NULL AND a.plugin_key||'/'||a.type = ANY(?)`, Strings(keys)), true)
}

// readQuotaRows runs db, the read of ReadAccountQuotas,
// ReadAccountQuotasOfTypes or AccountQuotasAt, and folds its rows; accounts
// lists the accounts covered (Accounts).
func readQuotaRows(db *gorm.DB, accounts bool) (*QuotaRead, error) {
	out := newQuotaRead(accounts)
	if err := EachRow(db, func(rows *sql.Rows) error { return out.add(rows) }); err != nil {
		return nil, err
	}
	return out, nil
}

// newQuotaRead is a QuotaRead of no rows yet; accounts makes Accounts the
// empty list rather than nil.
func newQuotaRead(accounts bool) *QuotaRead {
	out := &QuotaRead{Snapshots: map[int64]*QuotaSnapshot{}, Invalid: map[int64]error{}}
	if accounts {
		out.Accounts = []int64{}
	}
	return out
}

// add reads one row of the account column (NULL when not asked for) and
// selectQuotaRead into out.
func (out *QuotaRead) add(row RowScanner) error {
	var account, id *int64
	var q QuotaSnapshot
	var raw []byte
	if err := row.Scan(&account, &out.At, &id, &raw, &q.Source, &q.Error, &q.UpdatedAt, &q.LastPassiveAt, &q.LastActiveAt, &q.Revision, &q.NextCheckAt, &q.RefreshStatus, &q.Generation); err != nil {
		return err
	}
	if account != nil {
		out.Accounts = append(out.Accounts, *account)
	}
	if id == nil {
		return nil // no snapshot: the row only carries the time or the account
	}
	q.AccountID = *id
	var err error
	if q.Windows, err = quotaWindows(raw); err != nil {
		out.Invalid[q.AccountID] = err
		return nil
	}
	out.Snapshots[q.AccountID] = &q
	return nil
}

// SaveAccountQuota merges windows into the account's snapshot (a window
// overwrites the stored one with the same key, others are kept), records the
// source, clears the error and stamps updated_at (and last_passive_at for a
// passive sample) with the database clock. An account that no longer exists
// is skipped silently.
func (d *DB) SaveAccountQuota(ctx context.Context, accountID int64, source string, windows map[string]QuotaWindow, next ...time.Time) error {
	var nextAt *time.Time
	if len(next) > 0 {
		nextAt = &next[0]
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return err
	}
	return saveAccountQuota(G(ctx, d.Gorm), accountID, source, string(raw), nextAt).Error
}

func saveAccountQuota(db *gorm.DB, accountID int64, source, windows string, nextAt *time.Time) *gorm.DB {
	return db.Exec(`INSERT INTO account_quota_snapshots
			(account_id, windows, source, error, updated_at, last_passive_at, revision, refresh_status, next_check_at)
		SELECT ?::bigint, ?::jsonb, ?::text, '', clock_timestamp(), CASE WHEN ?::text = 'passive' THEN clock_timestamp() END, 1, 'ready', ?::timestamptz
		WHERE EXISTS (SELECT 1 FROM accounts WHERE id = ? AND deleted_at IS NULL)
		ON CONFLICT (account_id) DO UPDATE SET
			windows = account_quota_snapshots.windows || EXCLUDED.windows,
			source = EXCLUDED.source,
			error = '',
			updated_at = clock_timestamp(),
			revision = account_quota_snapshots.revision + 1,
			refresh_status = 'ready',
			next_check_at = CASE WHEN account_quota_snapshots.auto_refresh AND ?::timestamptz IS NOT NULL THEN LEAST(account_quota_snapshots.next_check_at,?::timestamptz) ELSE account_quota_snapshots.next_check_at END,
			last_passive_at = COALESCE(EXCLUDED.last_passive_at, account_quota_snapshots.last_passive_at)`,
		accountID, windows, source, source, nextAt, accountID, nextAt, nextAt)
}

func (d *DB) ApplyAccountQuotaObservation(ctx context.Context, accountID, expectedGeneration int64, pluginKey, accountType, pluginVersion string, windows map[string]QuotaWindow, next *time.Time) (bool, error) {
	if expectedGeneration <= 0 || len(windows) == 0 {
		return false, nil
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return false, err
	}
	res := applyQuotaObservation(G(ctx, d.Gorm), accountID, expectedGeneration, pluginKey, accountType, pluginVersion, string(raw), next)
	return res.Error == nil && res.RowsAffected == 1, res.Error
}

// applyQuotaObservation merges the windows document into the account's
// snapshot if the generation and the account's plugin still match. Every
// upstream answer with a quota observation runs it: Raw SQL through Exec,
// the GORM form that allocates least (BenchmarkHotQuotaObservation). It
// leaves updated_at as it is, as it always did.
func applyQuotaObservation(db *gorm.DB, accountID, expectedGeneration int64, pluginKey, accountType, pluginVersion, windows string, next *time.Time) *gorm.DB {
	return db.Exec(`UPDATE account_quota_snapshots q SET windows=q.windows||?::jsonb,
		source='passive',last_passive_at=clock_timestamp(),revision=q.revision+1,
		next_check_at=CASE WHEN q.auto_refresh AND ?::timestamptz IS NOT NULL THEN LEAST(q.next_check_at,?::timestamptz) ELSE q.next_check_at END
		WHERE q.account_id=? AND q.generation=? AND EXISTS(
			SELECT 1 FROM accounts a JOIN plugins p ON p.key=a.plugin_key
			WHERE a.id=q.account_id AND a.plugin_key=? AND a.type=? AND a.deleted_at IS NULL
			AND p.status IN ('enabled','upgrading') AND p.active_version=?)`,
		windows, next, next, accountID, expectedGeneration, pluginKey, accountType, pluginVersion)
}

// ClaimAccountQuotaQuery reserves the right to query the upstream for the
// account's quota: it stamps last_active_at and reports true, unless another
// attempt (on any node) did so less than floor ago. The row is created when
// missing.
func (d *DB) ClaimAccountQuotaQuery(ctx context.Context, accountID int64, floor time.Duration) (bool, error) {
	res := claimAccountQuotaQuery(G(ctx, d.Gorm), accountID, floor.Seconds())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func claimAccountQuotaQuery(db *gorm.DB, accountID int64, floor float64) *gorm.DB {
	return db.Exec(`INSERT INTO account_quota_snapshots (account_id, last_active_at)
		SELECT ?::bigint, now() WHERE EXISTS (SELECT 1 FROM accounts WHERE id = ?)
		ON CONFLICT (account_id) DO UPDATE SET last_active_at = now()
		WHERE account_quota_snapshots.last_active_at IS NULL
			OR account_quota_snapshots.last_active_at <= now() - make_interval(secs => ?::float8)`,
		accountID, accountID, floor)
}

// SetAccountQuotaError records the failure of an active query; the windows
// and updated_at are left as they were.
func (d *DB) SetAccountQuotaError(ctx context.Context, accountID int64, msg string) error {
	return setAccountQuotaError(G(ctx, d.Gorm), accountID, msg).Error
}

func setAccountQuotaError(db *gorm.DB, accountID int64, msg string) *gorm.DB {
	return db.Exec(`UPDATE account_quota_snapshots SET error = ?, refresh_status='error', revision=revision+1 WHERE account_id = ?`, msg, accountID)
}

type QuotaClaim struct {
	AccountID, Generation, Token, Revision int64
	Failures                               int
	PluginRevision                         string
	Initial                                bool
}

// EnsureAccountQuota creates the account's snapshot row, or aligns its plugin
// revision (a new one starts a new generation) and auto_refresh. It reports
// whether a row was written (inserted, or updated with a new revision): the
// caller announces the change so quota mirrors read it again.
func EnsureAccountQuota(ctx context.Context, q *gorm.DB, id int64, pluginRevision string, automatic bool) (bool, error) {
	res := ensureAccountQuota(G(ctx, q), id, pluginRevision, automatic)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ensureAccountQuotaSQL names each value once however often it is used.
var ensureAccountQuotaSQL = NewNumbered(`INSERT INTO account_quota_snapshots(account_id,plugin_revision,auto_refresh,next_check_at)
		SELECT id,$rev,$auto,CASE WHEN $auto THEN now() END FROM accounts WHERE id=$id AND deleted_at IS NULL
		ON CONFLICT(account_id) DO UPDATE SET plugin_revision=$rev,auto_refresh=$auto,
		generation=CASE WHEN account_quota_snapshots.plugin_revision<>$rev THEN nextval('account_quota_generation_seq') ELSE account_quota_snapshots.generation END,
		lease_until=CASE WHEN account_quota_snapshots.plugin_revision<>$rev THEN NULL ELSE account_quota_snapshots.lease_until END,
		revision=account_quota_snapshots.revision+1,
		next_check_at=CASE WHEN NOT $auto THEN NULL WHEN account_quota_snapshots.plugin_revision<>$rev THEN now() ELSE COALESCE(account_quota_snapshots.next_check_at,now()) END
		WHERE account_quota_snapshots.plugin_revision<>$rev OR account_quota_snapshots.auto_refresh<>$auto`, "id", "rev", "auto")

func ensureAccountQuota(db *gorm.DB, id int64, rev string, auto bool) *gorm.DB {
	return ensureAccountQuotaSQL.Exec(db, id, rev, auto)
}

func ResetAccountQuota(ctx context.Context, q *gorm.DB, id int64) error {
	return resetAccountQuota(G(ctx, q), id).Error
}

func resetAccountQuota(db *gorm.DB, id int64) *gorm.DB {
	return db.Exec(`UPDATE account_quota_snapshots SET windows='{}',source='',error='',updated_at=NULL,last_passive_at=NULL,
		generation=nextval('account_quota_generation_seq'),revision=revision+1,lease_until=NULL,failures=0,
		refresh_status='pending',next_check_at=CASE WHEN auto_refresh THEN now() END WHERE account_id=?`, id)
}

func (d *DB) LeaseAccountQuota(ctx context.Context, id int64, pluginRevision string, floor, lease time.Duration, force bool) (*QuotaClaim, error) {
	var c QuotaClaim
	err := leaseAccountQuota(G(ctx, d.Gorm), id, pluginRevision, floor.Seconds(), lease.Seconds(), force).Row().
		Scan(&c.AccountID, &c.Generation, &c.Token, &c.Revision, &c.Failures, &c.PluginRevision, &c.Initial)
	if IsNoRows(err) {
		return nil, nil
	}
	return &c, err
}

func leaseAccountQuota(db *gorm.DB, id int64, rev string, floor, lease float64, force bool) *gorm.DB {
	return db.Raw(`UPDATE account_quota_snapshots q SET last_active_at=clock_timestamp(),
		lease_token=nextval('account_quota_generation_seq'),lease_until=clock_timestamp()+make_interval(secs=>?::float8)
		WHERE account_id=? AND plugin_revision=?
		AND (lease_until IS NULL OR lease_until<=clock_timestamp())
		AND (?::float8<=0 OR last_active_at IS NULL OR last_active_at<=clock_timestamp()-make_interval(secs=>?::float8))
		AND (? OR next_check_at IS NULL OR next_check_at<=clock_timestamp())
		AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=q.account_id AND a.deleted_at IS NULL
		AND (? OR a.schedulable OR q.updated_at IS NULL)
		AND NOT EXISTS(SELECT 1 FROM plugins p WHERE p.key=a.plugin_key AND p.status NOT IN ('enabled','upgrading')))
		RETURNING account_id,generation,lease_token,revision,failures,plugin_revision,updated_at IS NULL`,
		lease, id, rev, floor, floor, force, force)
}

func (d *DB) AccountQuotaClaimValid(ctx context.Context, c *QuotaClaim) (bool, error) {
	var ok bool
	err := G(ctx, d.Gorm).Raw(`SELECT EXISTS(SELECT 1 FROM account_quota_snapshots q JOIN accounts a ON a.id=q.account_id
		WHERE q.account_id=? AND q.generation=? AND q.lease_token=? AND q.lease_until>clock_timestamp()
		AND a.deleted_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM plugins p WHERE p.key=a.plugin_key AND p.status NOT IN ('enabled','upgrading')))`, c.AccountID, c.Generation, c.Token).Row().Scan(&ok)
	return ok, err
}

func (d *DB) ReleaseAccountQuotaQuery(ctx context.Context, c *QuotaClaim, next time.Time) (bool, error) {
	res := releaseAccountQuota(G(ctx, d.Gorm), c, next)
	return res.Error == nil && res.RowsAffected == 1, res.Error
}

func releaseAccountQuota(db *gorm.DB, c *QuotaClaim, next time.Time) *gorm.DB {
	return db.Exec(`UPDATE account_quota_snapshots q SET lease_until=NULL,
		next_check_at=CASE WHEN revision<>? THEN next_check_at ELSE GREATEST(?::timestamptz,clock_timestamp()+interval '30 seconds') END
		WHERE account_id=? AND generation=? AND lease_token=? AND lease_until>clock_timestamp()
		AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=q.account_id AND a.deleted_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM plugins p WHERE p.key=a.plugin_key AND p.status NOT IN ('enabled','upgrading')))`,
		c.Revision, next, c.AccountID, c.Generation, c.Token)
}

func (d *DB) CompleteAccountQuota(ctx context.Context, c *QuotaClaim, windows map[string]QuotaWindow, msg string, next time.Time) (bool, error) {
	if windows == nil {
		windows = map[string]QuotaWindow{}
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return false, err
	}
	res := completeAccountQuota(G(ctx, d.Gorm), c, string(raw), msg, next)
	return res.Error == nil && res.RowsAffected == 1, res.Error
}

// completeAccountQuotaSQL names each value once however often it is used:
// msg and the claim's revision appear six and seven times.
var completeAccountQuotaSQL = NewNumbered(`UPDATE account_quota_snapshots q SET
		windows=CASE WHEN $msg='' AND revision=$rev THEN windows||$windows::jsonb ELSE windows END,
		source=CASE WHEN $msg='' AND revision=$rev THEN 'active' ELSE source END,
		updated_at=CASE WHEN $msg='' AND revision=$rev THEN clock_timestamp() ELSE updated_at END,
		error=CASE WHEN revision=$rev THEN $msg ELSE error END,
		refresh_status=CASE WHEN revision<>$rev THEN refresh_status WHEN $msg='' THEN 'ready' ELSE 'error' END,
		failures=CASE WHEN revision<>$rev THEN failures WHEN $msg='' THEN 0 ELSE LEAST(failures+1,10) END,
		next_check_at=CASE WHEN revision<>$rev THEN next_check_at ELSE $next::timestamptz END,lease_until=NULL,revision=revision+1
		WHERE account_id=$account AND generation=$generation AND lease_token=$token AND lease_until>clock_timestamp()
		AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=q.account_id AND a.deleted_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM plugins p WHERE p.key=a.plugin_key AND p.status NOT IN ('enabled','upgrading')))`, "msg", "rev", "windows", "next", "account", "generation", "token")

func completeAccountQuota(db *gorm.DB, c *QuotaClaim, windows, msg string, next time.Time) *gorm.DB {
	return completeAccountQuotaSQL.Exec(db, msg, c.Revision, windows, next, c.AccountID, c.Generation, c.Token)
}
