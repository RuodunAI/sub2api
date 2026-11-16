// Package authz implements RBAC: the permission catalog (core and plugin
// permissions), built-in roles, role management, the cached core.Authorizer,
// core.PermissionCatalog for the plugin runtime and the console menu. Who
// holds which grant is casbin_rule (CONTRACTS §99), read and written through
// package casbinrule; effective permission sets come from casbinx.
package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
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

	mu sync.RWMutex
	// minVersion is the newest authz version this node knows of: every
	// decision waits for the enforcer to hold it.
	minVersion int64
	// pending holds what each version this node bumped changed, until
	// Committed announces it.
	pending   map[int64]authzChange
	listeners []func()
	stepUp    core.StepUpVerifier
	// menuKeys are the permissions of each core menu item (BindMenuRoutes).
	menuKeys map[string][]string

	// casbin is the node's Casbin enforcer, which makes every decision (nil
	// without a database: tests that never decide).
	casbin casbinx.Enforcer
}

var (
	_ core.Authorizer        = (*Service)(nil)
	_ core.PermissionCatalog = (*Service)(nil)
	_ core.AuthzVersioner    = (*Service)(nil)
	_ core.ScopeGrantClearer = (*Service)(nil)
)

// New creates the service. Call Start before serving requests.
func New(d Deps) *Service {
	if d.PollInterval <= 0 {
		d.PollInterval = 30 * time.Second
	}
	s := &Service{db: d.DB, bus: d.Bus, plugins: d.Plugins, poll: d.PollInterval, pending: map[int64]authzChange{}}
	if d.DB != nil {
		s.casbin = casbinx.New(casbinx.NewLoader(d.DB), SelfPermissionKeys(), casbinx.Options{})
	}
	return s
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
		c, ok := parseChange(payload)
		if !ok {
			s.refreshVersion(context.Background())
			return
		}
		s.invalidate(c)
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
			if err := s.readVersion(ctx); err != nil {
				return core.TaskResult{}, err
			}
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

// Two counters in authz_meta (CONTRACTS §99.4): version, which every authz
// write moves, and policy_version, which only the writes of what the
// Casbin policy holds move (grants, permission statuses, role
// definitions). A write of a user's roles or status moves version alone
// and names the users: every node then reads those users again into its
// enforcer and drops their decisions only; the policy is not loaded again.

// authzChange is what one authz version changed, as authz:changed carries
// it (JSON).
type authzChange struct {
	Version int64 `json:"v"`
	// Policy is the policy version a policy write moved to; 0 when the
	// write changed users only.
	Policy int64 `json:"p,omitempty"`
	// Users are the users whose roles or status a user write changed; none
	// means every user (a write that does not say).
	Users []int64 `json:"u,omitempty"`
}

// parseChange reads an authz:changed payload; false when it is not one.
func parseChange(payload []byte) (authzChange, bool) {
	var c authzChange
	return c, json.Unmarshal(payload, &c) == nil && c.Version > 0
}

// Bump increments the authz version inside tx, for a write of the roles or
// status of users (none: of users it does not name, every user's set then
// drops), and returns the new value. It also takes the authz_meta row
// lock, serializing all authz mutations. Holding the lock, v-1 is the
// latest committed version, so entries this node cached before it are
// dropped: checks made inside tx never trust a cache that missed a
// broadcast.
func (s *Service) Bump(ctx context.Context, tx *gorm.DB, users ...int64) (int64, error) {
	return s.bump(ctx, tx, false, users)
}

// bumpPolicy is Bump for a write of what the policy holds: both versions
// move, and every node loads the policy again.
func (s *Service) bumpPolicy(ctx context.Context, tx *gorm.DB) (int64, error) {
	return s.bump(ctx, tx, true, nil)
}

func (s *Service) bump(ctx context.Context, tx *gorm.DB, policy bool, users []int64) (int64, error) {
	if err := store.RequireTx(tx); err != nil {
		return 0, err
	}
	m, err := bumpVersions(store.G(ctx, tx), policy)
	if err != nil {
		return 0, err
	}
	c := authzChange{Version: m.Version, Policy: m.PolicyVersion, Users: slices.Clone(users)}
	s.invalidate(authzChange{Version: c.Version - 1})
	s.mu.Lock()
	s.pending[c.Version] = c
	s.mu.Unlock()
	return c.Version, nil
}

// bumpVersions increments the authz version and, for a policy write, the
// policy version, locking their row, and returns them: the policy version
// is 0 when it stays (a user write).
func bumpVersions(db *gorm.DB, policy bool) (model.AuthzMeta, error) {
	set, returning := map[string]any{"version": gorm.Expr("version + 1")}, []clause.Column{{Name: "version"}}
	if policy {
		set["policy_version"] = gorm.Expr("policy_version + 1")
		returning = append(returning, clause.Column{Name: "policy_version"})
	}
	var m model.AuthzMeta
	res := db.Model(&m).Clauses(clause.Returning{Columns: returning}).Where("id = 1").Updates(set)
	if res.Error == nil && res.RowsAffected == 0 {
		return m, sql.ErrNoRows
	}
	return m, res.Error
}

// Committed must be called after a transaction that called Bump has
// committed: it invalidates local caches and broadcasts authz:changed with
// what the version changed (the users of a user write, or the policy).
func (s *Service) Committed(ctx context.Context, version int64) {
	if version == 0 {
		return
	}
	s.mu.Lock()
	c, ok := s.pending[version]
	delete(s.pending, version)
	s.mu.Unlock()
	if !ok {
		c = authzChange{Version: version}
	}
	s.invalidate(c)
	s.announce(ctx, c)
}

// announce tells every node, this one included, of c (authz:changed). A
// failure is logged, not returned: the version poll catches a lost
// message.
func (s *Service) announce(ctx context.Context, c authzChange) {
	if s.bus == nil {
		return
	}
	payload, _ := json.Marshal(c)
	if err := s.bus.Publish(ctx, core.ChannelAuthzChanged, payload); err != nil {
		slog.WarnContext(ctx, "authz: publish authz:changed failed", "err", err)
	}
}

// committedLater is used when the transaction belongs to the caller: it waits
// until the bumped version is visible, then notifies. If the transaction
// rolls back the wait times out and nothing is published (the poller keeps
// caches correct either way). A service without a database (tests) has
// nothing to wait on.
func (s *Service) committedLater(version int64) {
	if s.db == nil {
		return
	}
	go func() {
		ctx := context.Background()
		deadline := time.Now().Add(2 * time.Minute)
		delay := 50 * time.Millisecond
		for time.Now().Before(deadline) {
			time.Sleep(delay)
			if delay < time.Second {
				delay *= 2
			}
			v, _, err := s.currentVersions(ctx)
			if err == nil && v >= version {
				s.Committed(ctx, version)
				return
			}
		}
	}()
}

// mutate runs fn in a transaction after bumping both versions (every
// write through it changes what the policy holds: grants, role
// definitions), then notifies.
func (s *Service) mutate(ctx context.Context, fn func(tx *gorm.DB) error) error {
	var v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var err error
		if v, err = s.bumpPolicy(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return err
	}
	s.Committed(ctx, v)
	return nil
}

// mutateRole is mutate for a write of one role, whose id fn returns: the
// role is read back in the same transaction, after fn, so that what the
// write answers (its revision above all, which the console sends with its
// next write) is what this write made, never someone else's change
// committed after it.
func (s *Service) mutateRole(ctx context.Context, fn func(tx *gorm.DB) (int64, error)) (*Role, error) {
	var r *Role
	err := s.mutate(ctx, func(tx *gorm.DB) error {
		id, err := fn(tx)
		if err == nil {
			r, err = s.getRole(ctx, tx, id)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// errVersionConflict is the refusal of a role write made against a role
// revision other than the current one (expected, CONTRACTS C1): the editor
// read before someone else's change to the role, which the write would
// undo.
func errVersionConflict(ctx context.Context, current int64) error {
	return core.NewError(http.StatusConflict, "version_conflict", t(ctx,
		"the role changed since you loaded it; reload and save again",
		"角色在你打开之后已被修改，请重新加载后再保存")).WithDetails(map[string]any{"revision": current})
}

// currentVersions reads the authz and policy versions.
func (s *Service) currentVersions(ctx context.Context) (int64, int64, error) {
	return store.AuthzVersions(store.G(ctx, s.db.Gorm))
}

// readVersion reads the version and takes it in: a version this node
// missed has the enforcer load the policy again.
func (s *Service) readVersion(ctx context.Context) error {
	v, _, err := s.currentVersions(ctx)
	if err != nil {
		return err
	}
	s.invalidate(authzChange{Version: v})
	return nil
}

func (s *Service) refreshVersion(ctx context.Context) {
	if err := s.readVersion(ctx); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "authz: version check failed", "err", err)
	}
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

// invalidate takes in that version c.Version changed what c says: the
// enforcer applies it (the users it names alone when it is the version
// next to the enforcer's, else by loading the policy again), then this
// node knows of the version, and so waits for it on every decision.
func (s *Service) invalidate(c authzChange) {
	if s.casbin != nil {
		// Told first: a decision that reads the new version finds the
		// enforcer told of it.
		s.casbin.Changed(casbinx.Change(c))
	}
	s.mu.Lock()
	if c.Version <= s.minVersion {
		s.mu.Unlock()
		return
	}
	s.minVersion = c.Version
	for v := range s.pending {
		if v <= c.Version {
			delete(s.pending, v)
		}
	}
	listeners := append([]func(){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range listeners {
		go fn()
	}
}

// ------------------------------------------------------------ core.Authorizer

// errNoEnforcer is the answer of a service without a database: it has no
// enforcer to decide with.
var errNoEnforcer = errors.New("authz: no database to decide permissions with")

// Can is Casbin's decision: Enforce of permission for userID, through its
// decision cache, at this node's version; under an access key, of the key's
// narrowed set (PermissionSet). A decision the enforcer could not make is
// an error (unavailable), never a denial.
func (s *Service) Can(ctx context.Context, userID int64, permission string) (bool, error) {
	if core.ByAccessKey(ctx, userID) {
		set, err := s.PermissionSet(ctx, userID)
		return err == nil && set.Has(permission), err
	}
	if s.casbin == nil {
		return false, errNoEnforcer
	}
	ok, err := s.casbin.Enforce(ctx, userID, s.Version(), permission)
	return ok, unavailable(err)
}

// PermissionSet returns what userID may do in this request, at this node's
// version: Casbin's lists of the user's grants, its Has Casbin's Enforce
// (which reads nothing more: casbinx); under an access key the key's
// permissions, and never more than the user's.
func (s *Service) PermissionSet(ctx context.Context, userID int64) (core.PermissionSet, error) {
	if s.casbin == nil {
		return core.PermissionSet{}, errNoEnforcer
	}
	set, err := s.casbin.Permissions(ctx, userID, s.Version())
	if err != nil {
		return core.PermissionSet{}, unavailable(err)
	}
	return core.RestrictToAccessKey(ctx, userID, set), nil
}

// unavailable is the error of a decision the enforcer could not make: it
// did not reach the version this node knows of in time (casbinx.ErrStale),
// or could not read the user. 503, its cause logged with the request
// (httpapi.Fail), so that a database fault never reads as a denial.
func unavailable(err error) error {
	if err == nil {
		return nil
	}
	return core.ErrUnavailable.WithCause(err)
}

// RolesPermissions returns the union of all permissions granted by the given
// role keys, in the grant view: disabled (plugin) permissions count, removed
// ones do not (M6). It also holds the roles they may assign: those listed,
// never a superuser role, plus, when one of them has assign_custom, every
// custom role but roleKeys themselves. The wildcard never covers a role the
// holder holds (CONTRACTS §43.6): a custom role with assign_custom does not
// manage itself or its peers, nor hand out what does; a role listed by name
// stays assignable as listed. Used to compare what roles carry in grant
// checks; the effective set (Casbin's, PermissionSet) counts active permissions
// only.
func (s *Service) RolesPermissions(ctx context.Context, q *gorm.DB, roleKeys []string) (core.PermissionSet, error) {
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
func (s *Service) eachRolePermissions(ctx context.Context, q *gorm.DB, roleKeys []string) (map[string]core.PermissionSet, error) {
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
func readRoleGrants(ctx context.Context, q *gorm.DB, roleKeys []string) (map[string]*roleGrantRow, []string, error) {
	out := map[string]*roleGrantRow{}
	db := store.G(ctx, q)
	var role string
	var su, custom, anyCustom bool
	var key sql.NullString
	if err := store.EachRow(roleGrants(db, roleKeys), func(rows *sql.Rows) error {
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
	if err := store.EachRow(roleAssignables(db, roleKeys), func(rows *sql.Rows) error {
		if err := rows.Scan(&role, &assignable); err != nil {
			return err
		}
		// The two reads may see different snapshots (q a pool): a role
		// created between them has no grant row read.
		if r := out[role]; r != nil {
			r.assignable = append(r.assignable, assignable)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if !anyCustom {
		return out, nil, nil
	}
	customs, err := customRoleKeys(db, "")
	if err != nil {
		return nil, nil, err
	}
	return out, customs, nil
}

// roleGrants selects a row for each grant of each role of roleKeys (its
// key, flags and a canonical key of the grant view: removed and staged
// permissions aside), one of NULL grant for a role without any.
func roleGrants(db *gorm.DB, roleKeys []string) *gorm.DB {
	grants := db.Table("(" + casbinrule.GrantsOfRole + ") rg").Select("rg.key").
		Joins("JOIN permissions p ON p.key = rg.action AND p.status NOT IN ('removed', 'staged')")
	return db.Table("roles r").Select("r.key, r.superuser, r.assign_custom, g.key").
		Joins("LEFT JOIN LATERAL (?) g ON true", grants).Where("r.key = ANY(?)", store.Strings(roleKeys))
}

// roleAssignables selects the (role, assignable role) pairs of the
// assignable lists of roleKeys, superuser roles aside.
func roleAssignables(db *gorm.DB, roleKeys []string) *gorm.DB {
	return db.Table("role_assignable_roles ra").Select("r.key, ar.key").
		Joins("JOIN roles r ON r.id = ra.role_id AND r.key = ANY(?)", store.Strings(roleKeys)).
		Joins("JOIN roles ar ON ar.id = ra.assignable_role_id AND NOT ar.superuser")
}

// customRoleKeys returns the keys of the custom roles but the role but.
func customRoleKeys(db *gorm.DB, but string) ([]string, error) {
	keys := []string{}
	err := db.Model(&model.Role{}).Where("NOT builtin AND key <> ?", but).Pluck("key", &keys).Error
	return keys, err
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
// included (used for superusers in /me), read when asked: no request path
// needs the catalog.
func (s *Service) ActivePermissionKeys(ctx context.Context) ([]string, error) {
	var keys []string
	if err := store.G(ctx, s.db.Gorm).Model(&model.Permission{}).Where("status = 'active'").Pluck("key", &keys).Error; err != nil {
		return nil, err
	}
	return append(SelfPermissionKeys(), keys...), nil
}
