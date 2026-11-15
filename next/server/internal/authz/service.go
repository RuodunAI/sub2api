// Package authz implements RBAC: the permission catalog (core and plugin
// permissions), built-in roles, role management, the cached core.Authorizer,
// core.PermissionCatalog for the plugin runtime and the console menu.
package authz

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Deps are the dependencies of Service.
type Deps struct {
	DB  *store.DB
	Bus core.Bus
	// Plugins supplies plugin menus for /me/menus; may be nil.
	Plugins core.PluginRegistry
	// PollInterval is the fallback version check period (default 30s).
	PollInterval time.Duration
}

// Service is the authz module. It implements core.Authorizer and
// core.PermissionCatalog.
type Service struct {
	db      *store.DB
	bus     core.Bus
	plugins core.PluginRegistry
	poll    time.Duration

	mu         sync.RWMutex
	minVersion int64 // cached entries older than this are stale
	users      map[int64]core.PermissionSet
	catalog    *catalogSnapshot
	listeners  []func()
	stepUp     core.StepUpVerifier
	// menuKeys are the permissions of each core menu item (BindMenuRoutes).
	menuKeys map[string][]string
}

type permMeta struct {
	status string
}

type catalogSnapshot struct {
	version int64
	perms   map[string]permMeta
}

var (
	_ core.Authorizer        = (*Service)(nil)
	_ core.PermissionCatalog = (*Service)(nil)
	_ core.AuthzVersioner    = (*Service)(nil)
)

// New creates the service. Call Start before serving requests.
func New(d Deps) *Service {
	if d.PollInterval <= 0 {
		d.PollInterval = 30 * time.Second
	}
	return &Service{db: d.DB, bus: d.Bus, plugins: d.Plugins, poll: d.PollInterval, users: map[int64]core.PermissionSet{}}
}

// Start syncs the core catalog and built-in roles and subscribes to
// authz:changed and bus reconnects (stopped with ctx); the fallback version
// poll is the node task RegisterTasks registers.
func (s *Service) Start(ctx context.Context) error {
	if err := s.SyncCore(ctx); err != nil {
		return err
	}
	// Version answers from the first request on, not from the first change.
	s.refreshVersion(ctx)
	if s.bus == nil {
		return nil
	}
	cancels := []func(){s.bus.Subscribe(core.ChannelAuthzChanged, func(payload []byte) {
		v, err := strconv.ParseInt(string(payload), 10, 64)
		if err != nil {
			s.refreshVersion(context.Background())
			return
		}
		s.invalidate(v)
	})}
	if r, ok := s.bus.(core.BusReconnects); ok {
		// Broadcasts sent while disconnected are lost: read the version again
		// (off the bus goroutine, which must not block).
		cancels = append(cancels, r.OnReconnect(func() { go s.refreshVersion(context.Background()) }))
	}
	go func() {
		<-ctx.Done()
		for _, cancel := range cancels {
			cancel()
		}
	}()
	return nil
}

// VersionPollTask is the node task that reads the authz version, in case an
// authz:changed broadcast was missed.
const VersionPollTask = "core.authz.version_poll"

// RegisterTasks registers the version poll (every PollInterval, on every
// node from start).
func (s *Service) RegisterTasks(tasks core.Tasks) error {
	return tasks.Register(core.TaskSpec{
		Name:  VersionPollTask,
		Title: core.LocalizedText{"en": "Refresh the permission version", "zh": "刷新权限版本"},
		Description: core.LocalizedText{
			"en": "Reads the permission version so this node drops cached permissions even when it missed a change broadcast.",
			"zh": "读取权限版本，即使本节点错过了变更广播，也会丢弃过期的权限缓存。",
		},
		Scope:   core.TaskNode,
		Every:   s.poll,
		Timeout: 10 * time.Second,
		Handler: func(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
			v, err := s.currentVersion(ctx)
			if err != nil {
				return core.TaskResult{}, err
			}
			s.invalidate(v)
			return core.TaskResult{}, nil
		},
	})
}

// OnChange registers fn to run (asynchronously) whenever the authz version
// advances on this node; iam uses it to drop its user status cache.
func (s *Service) OnChange(fn func()) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// ------------------------------------------------------------ versioning

// Bump increments the authz version inside tx and returns the new value. It
// also takes the authz_meta row lock, serializing all authz mutations.
// Holding the lock, v-1 is the latest committed version, so entries this node
// cached before it are dropped: checks made inside tx never trust a cache
// that missed a broadcast.
func (s *Service) Bump(ctx context.Context, tx pgx.Tx) (int64, error) {
	var v int64
	if err := bumpVersion(store.G(ctx, tx)).Row().Scan(&v); err != nil {
		return 0, err
	}
	s.invalidate(v - 1)
	return v, nil
}

// bumpVersion increments the authz version, locking its row, and returns
// the new value.
func bumpVersion(db *gorm.DB) *gorm.DB {
	return db.Raw(`UPDATE authz_meta SET version = version + 1 WHERE id = 1 RETURNING version`)
}

// readVersion selects the authz version.
func readVersion(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT version FROM authz_meta WHERE id = 1`)
}

// Committed must be called after a transaction that called Bump has
// committed: it invalidates local caches and broadcasts authz:changed.
func (s *Service) Committed(ctx context.Context, version int64) {
	s.invalidate(version)
	core.AnnounceAuthzChanged(ctx, s.bus, version)
}

// committedLater is used when the transaction belongs to the caller: it waits
// until the bumped version is visible, then notifies. If the transaction
// rolls back the wait times out and nothing is published (the poller keeps
// caches correct either way).
func (s *Service) committedLater(version int64) {
	go func() {
		ctx := context.Background()
		deadline := time.Now().Add(2 * time.Minute)
		delay := 50 * time.Millisecond
		for time.Now().Before(deadline) {
			time.Sleep(delay)
			if delay < time.Second {
				delay *= 2
			}
			v, err := s.currentVersion(ctx)
			if err == nil && v >= version {
				s.Committed(ctx, v)
				return
			}
		}
	}()
}

// mutate runs fn in a transaction after bumping the version, then notifies.
func (s *Service) mutate(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return s.mutateAt(ctx, nil, fn)
}

// errVersionConflict is the refusal of a write made against an authz
// version other than the current one (expected, CONTRACTS C1): the editor
// read before someone else's change, which the write would undo.
func errVersionConflict(ctx context.Context, current int64) error {
	return core.NewError(http.StatusConflict, "version_conflict", t(ctx,
		"permissions changed since you loaded them; reload and save again",
		"权限在你打开之后已被修改，请重新加载后再保存")).WithDetails(map[string]any{"version": current})
}

// mutateAt is mutate failing with errVersionConflict, before fn runs, when
// expected is set and is not the authz version the write starts from (the
// Bump serializes the comparison with every other authz write).
func (s *Service) mutateAt(ctx context.Context, expected *int64, fn func(tx pgx.Tx) error) error {
	var v int64
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if v, err = s.Bump(ctx, tx); err != nil {
			return err
		}
		if expected != nil && *expected != v-1 {
			return errVersionConflict(ctx, v-1)
		}
		return fn(tx)
	})
	if err != nil {
		return err
	}
	s.Committed(ctx, v)
	return nil
}

func (s *Service) currentVersion(ctx context.Context) (int64, error) {
	var v int64
	err := readVersion(store.G(ctx, s.db.Pool)).Row().Scan(&v)
	return v, err
}

func (s *Service) refreshVersion(ctx context.Context) {
	v, err := s.currentVersion(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "authz: version check failed", "err", err)
		}
		return
	}
	s.invalidate(v)
}

// Version implements core.AuthzVersioner: the newest authz version this node
// has seen (at start, in its own changes, in authz:changed or in the version
// poll). It reads no database, so it may trail another node's change by the
// time the broadcast takes.
func (s *Service) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.minVersion
}

// invalidate drops cache entries older than version.
func (s *Service) invalidate(version int64) {
	s.mu.Lock()
	if version <= s.minVersion {
		s.mu.Unlock()
		return
	}
	s.minVersion = version
	for id, set := range s.users {
		if set.Version < version {
			delete(s.users, id)
		}
	}
	listeners := append([]func(){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range listeners {
		go fn()
	}
}

// ------------------------------------------------------------ core.Authorizer

func (s *Service) Can(ctx context.Context, userID int64, permission string) (bool, error) {
	set, err := s.PermissionSet(ctx, userID)
	if err != nil {
		return false, err
	}
	return set.Has(permission), nil
}

// PermissionSet returns what userID may do in this request: under an access key
// the key's permissions, and never more than the user's.
func (s *Service) PermissionSet(ctx context.Context, userID int64) (core.PermissionSet, error) {
	set, err := s.userPermissionSet(ctx, userID)
	if err != nil {
		return core.PermissionSet{}, err
	}
	return core.RestrictToAccessKey(ctx, userID, set), nil
}

func (s *Service) userPermissionSet(ctx context.Context, userID int64) (core.PermissionSet, error) {
	if err := s.ensureCatalog(ctx); err != nil {
		return core.PermissionSet{}, err
	}
	s.mu.RLock()
	set, ok := s.users[userID]
	min := s.minVersion
	s.mu.RUnlock()
	if ok && set.Version >= min {
		return set, nil
	}
	set, err := s.loadPermissionSet(ctx, userID)
	if err != nil {
		return core.PermissionSet{}, err
	}
	s.mu.Lock()
	if set.Version >= s.minVersion {
		s.users[userID] = set
	}
	s.mu.Unlock()
	return set, nil
}

// readSnapshot runs fn in a read-only repeatable-read transaction and returns
// the authz version seen by that snapshot.
func (s *Service) readSnapshot(ctx context.Context, fn func(tx pgx.Tx) error) (int64, error) {
	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var v int64
	if err := readVersion(store.G(ctx, tx)).Row().Scan(&v); err != nil {
		return 0, err
	}
	if err := fn(tx); err != nil {
		return 0, err
	}
	return v, tx.Commit(ctx)
}

func (s *Service) loadPermissionSet(ctx context.Context, userID int64) (core.PermissionSet, error) {
	set := core.PermissionSet{Keys: map[string]struct{}{}}
	v, err := s.readSnapshot(ctx, func(tx pgx.Tx) error {
		// One row at least for an active user, roles or not: a signed-in
		// user holds the self permissions (withSelf).
		active := false
		var su bool
		var key sql.NullString
		if err := eachRow(userGrants(store.G(ctx, tx), userID), func(rows *sql.Rows) error {
			if err := rows.Scan(&su, &key); err != nil {
				return err
			}
			active = true
			if su {
				set.Superuser = true
			}
			if key.Valid {
				set.Add(key.String)
			}
			return nil
		}); err != nil {
			return err
		}
		if active {
			set = withSelf(set)
		}
		return nil
	})
	if err != nil {
		return core.PermissionSet{}, err
	}
	set.Version = v
	return set, nil
}

// userGrantsSQL selects a row for each grant of each role of an active
// user (canonical keys of active permissions) with whether the role is a
// superuser one: one row of NULLs, superuser false, for a user without
// roles, none for a user who is not active.
var userGrantsSQL = `
SELECT COALESCE(r.superuser, false), g.key
FROM users u
LEFT JOIN user_roles ur ON ur.user_id = u.id
LEFT JOIN roles r ON r.id = ur.role_id` + roleGrantsJoin(`p.status = 'active'`) + `
WHERE u.id = ? AND u.deleted_at IS NULL AND u.status = 'active'`

func userGrants(db *gorm.DB, userID int64) *gorm.DB {
	return db.Raw(userGrantsSQL, userID)
}

// eachRow runs db, a query, and calls scan on each row it returns: rows
// read by position (columns without names of their own, or the same name
// twice) and folded as they come, never collected.
func eachRow(db *gorm.DB, scan func(*sql.Rows) error) error {
	rows, err := db.Rows()
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// RolesPermissions returns the union of all permissions granted by the given
// role keys, in the grant view: disabled (plugin) permissions count, removed
// ones do not (M6). It also holds the roles they may assign: those listed,
// never a superuser role, plus, when one of them has assign_custom, every
// custom role but roleKeys themselves. The wildcard never covers a role the
// holder holds (CONTRACTS §43.6): a custom role with assign_custom does not
// manage itself or its peers, nor hand out what does; a role listed by name
// stays assignable as listed. Used to compare what roles carry in grant
// checks; the effective set (loadPermissionSet) counts active permissions
// only.
func (s *Service) RolesPermissions(ctx context.Context, q store.Querier, roleKeys []string) (core.PermissionSet, error) {
	set := core.PermissionSet{Keys: map[string]struct{}{}, Assignable: map[string]struct{}{}}
	if len(roleKeys) == 0 {
		return set, nil
	}
	roles, customs, err := readRoleGrants(ctx, q, roleKeys)
	if err != nil {
		return core.PermissionSet{}, err
	}
	for _, r := range roles {
		set.Superuser = set.Superuser || r.superuser
		set.AssignCustom = set.AssignCustom || r.assignCustom
		for _, k := range r.keys {
			set.Add(k)
		}
		for _, k := range r.assignable {
			set.Assignable[k] = struct{}{}
		}
	}
	if set.AssignCustom {
		addCustoms(set.Assignable, customs, roleKeys)
	}
	return set, nil
}

// eachRolePermissions is RolesPermissions of each role of roleKeys alone,
// keyed by role key (absent for an unknown one), in at most three queries however
// many roles: what editDenial compares each role of a listing by.
func (s *Service) eachRolePermissions(ctx context.Context, q store.Querier, roleKeys []string) (map[string]core.PermissionSet, error) {
	roles, customs, err := readRoleGrants(ctx, q, roleKeys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]core.PermissionSet, len(roles))
	for key, r := range roles {
		set := core.PermissionSetOf(r.keys...)
		set.Superuser, set.AssignCustom = r.superuser, r.assignCustom
		set.Assignable = core.PermissionSetOf(r.assignable...).Keys
		if r.assignCustom {
			addCustoms(set.Assignable, customs, []string{key})
		}
		out[key] = set
	}
	return out, nil
}

// roleGrantRow is one role as readRoleGrants reads it: its flags, its grants
// (canonical keys, removed and staged permissions aside) and the roles its assignable
// list names, superuser roles aside.
type roleGrantRow struct {
	superuser, assignCustom bool
	keys, assignable        []string
}

// readRoleGrants reads each role of roleKeys (absent for an unknown one)
// and, when one has assign_custom, every custom role key: the one reading of
// what roles carry behind RolesPermissions and eachRolePermissions, which
// differ only in whom the assign_custom wildcard leaves out (addCustoms).
func readRoleGrants(ctx context.Context, q store.Querier, roleKeys []string) (map[string]*roleGrantRow, []string, error) {
	out := map[string]*roleGrantRow{}
	db := store.G(ctx, q)
	var role string
	var su, custom, anyCustom bool
	var key sql.NullString
	if err := eachRow(roleGrants(db, roleKeys), func(rows *sql.Rows) error {
		if err := rows.Scan(&role, &su, &custom, &key); err != nil {
			return err
		}
		r := out[role]
		if r == nil {
			r = &roleGrantRow{superuser: su, assignCustom: custom}
			out[role] = r
			anyCustom = anyCustom || custom
		}
		if key.Valid {
			r.keys = append(r.keys, key.String)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	var assignable string
	if err := eachRow(roleAssignables(db, roleKeys), func(rows *sql.Rows) error {
		if err := rows.Scan(&role, &assignable); err != nil {
			return err
		}
		out[role].assignable = append(out[role].assignable, assignable)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if !anyCustom {
		return out, nil, nil
	}
	var customs store.Strings
	if err := customRoleKeys(db, "").Row().Scan(&customs); err != nil {
		return nil, nil, err
	}
	return out, customs, nil
}

// roleGrants selects a row for each grant of each role of roleKeys (its
// key, flags and a canonical key of the grant view: removed and staged
// permissions aside), one of NULL grant for a role without any.
func roleGrants(db *gorm.DB, roleKeys []string) *gorm.DB {
	return db.Raw(`
SELECT r.key, r.superuser, r.assign_custom, g.key
FROM roles r`+roleGrantsJoin(`p.status NOT IN ('removed', 'staged')`)+`
WHERE r.key = ANY(?)`, store.Strings(roleKeys))
}

// roleAssignables selects the (role, assignable role) pairs of the
// assignable lists of roleKeys, superuser roles aside.
func roleAssignables(db *gorm.DB, roleKeys []string) *gorm.DB {
	return db.Raw(`
SELECT r.key, ar.key FROM role_assignable_roles ra
JOIN roles r ON r.id = ra.role_id AND r.key = ANY(?)
JOIN roles ar ON ar.id = ra.assignable_role_id AND NOT ar.superuser`, store.Strings(roleKeys))
}

// customRoleKeys selects the keys of the custom roles as one array, leaving
// out except unless it is "".
func customRoleKeys(db *gorm.DB, except string) *gorm.DB {
	if except == "" {
		return db.Raw(`SELECT COALESCE(array_agg(key), '{}') FROM roles WHERE NOT builtin`)
	}
	return db.Raw(`SELECT COALESCE(array_agg(key), '{}') FROM roles WHERE NOT builtin AND key <> ?`, except)
}

// addCustoms adds to assignable every custom role but those of except: what
// the assign_custom wildcard of except's holder covers (§43.6).
func addCustoms(assignable map[string]struct{}, customs, except []string) {
	for _, k := range customs {
		if !slices.Contains(except, k) {
			assignable[k] = struct{}{}
		}
	}
}

// ActivePermissionKeys lists every active permission, the self ones
// included (used for superusers in /me).
func (s *Service) ActivePermissionKeys(ctx context.Context) ([]string, error) {
	if err := s.ensureCatalog(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := SelfPermissionKeys()
	for k, m := range s.catalog.perms {
		if m.status == "active" {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *Service) ensureCatalog(ctx context.Context) error {
	s.mu.RLock()
	fresh := s.catalog != nil && s.catalog.version >= s.minVersion
	s.mu.RUnlock()
	if fresh {
		return nil
	}
	snap := &catalogSnapshot{perms: map[string]permMeta{}}
	v, err := s.readSnapshot(ctx, func(tx pgx.Tx) error {
		return eachRow(catalogRows(store.G(ctx, tx)), func(rows *sql.Rows) error {
			var k string
			var m permMeta
			if err := rows.Scan(&k, &m.status); err != nil {
				return err
			}
			snap.perms[k] = m
			return nil
		})
	})
	if err != nil {
		return err
	}
	snap.version = v
	s.mu.Lock()
	if s.catalog == nil || s.catalog.version < v {
		s.catalog = snap
	}
	s.mu.Unlock()
	return nil
}

// catalogRows selects the key and status of every permission.
func catalogRows(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT key, status FROM permissions`)
}
