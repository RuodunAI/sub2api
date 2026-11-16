package ccgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Draft runtimes (CONTRACTS §49.9): the account editor starts a runtime
// before the account exists, authorizes Claude Code in it, and POST
// /accounts adopts it. A draft is only visible to its creator unless the
// caller holds settings:manage.

// draftActions are the permissions of the draft endpoints: settings:manage,
// or account:create with any scope: whoever may create some accounts (POST
// /accounts) must be able to prepare the runtime of a Claude Code account
// too.
var draftActions = []perm.Key{perm.SettingsManage, perm.AccountCreate}

func (s *Service) registerDraftRoutes(r *httpapi.Router) {
	r.PermScoped("POST", "/system/ccgateway/drafts", s.draftCreate, draftActions...)
	r.PermScoped("PUT", "/system/ccgateway/drafts/:key", s.draftUpdate, draftActions...)
	r.PermScoped("DELETE", "/system/ccgateway/drafts/:key", s.draftDelete, draftActions...)
	r.PermScoped("GET", "/system/ccgateway/drafts/:key/:action", s.draftAction, draftActions...)
	r.PermScoped("POST", "/system/ccgateway/drafts/:key/:action", s.draftAction, draftActions...)
}

func newDraftKey() string {
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e) // crypto/rand never fails on supported platforms
	}
	return "d" + hex.EncodeToString(b[:])
}

// draftScope is nil for settings administrators (every draft), otherwise
// the caller id (only drafts it created).
func draftScope(ctx context.Context) *int64 {
	if core.HasGranted(ctx, perm.SettingsManage) {
		return nil
	}
	uid, _ := core.UserID(ctx)
	return &uid
}

func draftNotFound(base *core.Error) *core.Error { return reasonError(base, "draft_not_found") }

// Draft kinds of touchDraftOf: every open draft, or only the drafts of new
// accounts (for_account NULL); a positive value is the account whose
// re-authorization draft is wanted (§49.17).
const (
	anyDraft     int64 = -1
	accountDraft int64 = 0
)

// draftKind keeps the drafts of kind (anyDraft, accountDraft or an account
// id).
func draftKind(kind int64) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if kind == anyDraft {
			return db
		}
		return db.Where("COALESCE(for_account, 0) = ?", kind)
	}
}

// touchDraft marks an open draft the caller may see as used (last_seen_at)
// and reports whether it exists. Adopted drafts are accounts now and
// retired runtimes are gone for the API: not found.
func (s *Service) touchDraft(ctx context.Context, key string, scope *int64) (bool, error) {
	return s.touchDraftOf(ctx, key, scope, anyDraft)
}

// touchDraftOf is touchDraft restricted to one kind of draft (anyDraft,
// accountDraft or an account id).
func (s *Service) touchDraftOf(ctx context.Context, key string, scope *int64, kind int64) (bool, error) {
	if !isDraftKey(key) {
		return false, nil
	}
	res := openDraft(store.G(ctx, s.DB.Gorm), key).Scopes(createdBy(scope), draftKind(kind)).
		Update("last_seen_at", gorm.Expr("now()"))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// checkProxy validates a draft's proxy_id: none (direct egress), or visible
// to the caller and not disabled.
func (s *Service) checkProxy(ctx context.Context, id *int64) error {
	if id == nil {
		return nil
	}
	if *id <= 0 {
		return reasonError(core.ErrInvalidArgument, "proxy_not_found")
	}
	// The account module's rule for POST /accounts (CONTRACTS §21.4).
	refs, e := core.ProxyRefScope(ctx, s.Authorizer, perm.AccountCreate)
	if e != nil {
		return e
	}
	var status string
	e = store.G(ctx, s.DB.Gorm).Table("proxies p").Select("p.status").Where("p.id = ?", *id).
		Scopes(core.ProxyRows.Scope(refs)).Take(&status).Error
	if store.IsNoRows(e) {
		return reasonError(core.ErrInvalidArgument, "proxy_not_found")
	}
	if e != nil {
		return e
	}
	if status == "disabled" {
		return reasonError(core.ErrInvalidArgument, "proxy_disabled")
	}
	return nil
}

func (s *Service) runtimesOn(ctx context.Context) bool {
	cfg, e := s.Load(ctx)
	return e == nil && cfg.AccountRuntimes
}

type draftInput struct {
	ProxyID *int64 `json:"proxy_id"`
}

// draftCreate serves POST /system/ccgateway/drafts {proxy_id} → 201 {key}.
func (s *Service) draftCreate(c *gin.Context) {
	httpapi.NoStore(c)
	ctx := c.Request.Context()
	var in draftInput
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if !s.runtimesOn(ctx) {
		httpapi.Fail(c, reasonError(core.ErrUnavailable, "not_configured"))
		return
	}
	if e := s.checkProxy(ctx, in.ProxyID); e != nil {
		httpapi.Fail(c, e)
		return
	}
	uid, _ := core.UserID(ctx)
	key := newDraftKey()
	// A draft takes a runtime container until it is adopted, deleted or
	// swept; a settings administrator is not limited. The count and the
	// insert run under the user's lock.
	limit := core.MaxOpenDrafts
	if draftScope(ctx) == nil {
		limit = 0 // no limit
	}
	e := s.DB.Tx(ctx, func(tx *gorm.DB) error {
		if limit > 0 {
			if err := store.XactLock(ctx, tx, core.OpenDraftsLock(uid)); err != nil {
				return err
			}
		}
		// The draft is inserted unless uid has limit open drafts already (0:
		// no limit). Positional: GORM leaves a named parameter followed by a
		// cast (@uid::bigint) unreplaced.
		res := store.G(ctx, tx).Exec(`INSERT INTO ccgateway_runtimes(key, proxy_id, created_by)
			SELECT ?, ?, NULLIF(?::bigint, 0)
			WHERE ?::int = 0 OR (SELECT count(*) FROM ccgateway_runtimes
				WHERE created_by = ? AND account_id IS NULL AND retired_at IS NULL) < ?`,
			key, in.ProxyID, uid, limit, uid, limit)
		if res.Error == nil && res.RowsAffected == 0 {
			return core.ErrTooManyDrafts
		}
		return res.Error
	})
	if e != nil {
		httpapi.Fail(c, e)
		return
	}
	s.Kick(key)
	s.record(c, "draft."+key+".create")
	httpapi.Created(c, gin.H{"key": key})
}

// draftUpdate serves PUT /system/ccgateway/drafts/:key {proxy_id} → {key}.
// A re-authorization draft keeps the account's proxy: not found.
func (s *Service) draftUpdate(c *gin.Context) {
	httpapi.NoStore(c)
	ctx := c.Request.Context()
	key := c.Param("key")
	scope := draftScope(ctx)
	if ok, e := s.touchDraftOf(ctx, key, scope, accountDraft); e != nil || !ok {
		httpapi.Fail(c, errOr(e, draftNotFound(core.ErrNotFound)))
		return
	}
	var in draftInput
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if e := s.checkProxy(ctx, in.ProxyID); e != nil {
		httpapi.Fail(c, e)
		return
	}
	res := setDraftProxy(store.G(ctx, s.DB.Gorm), key, scope, in.ProxyID)
	if res.Error != nil || res.RowsAffected != 1 {
		httpapi.Fail(c, errOr(res.Error, draftNotFound(core.ErrNotFound)))
		return
	}
	s.Kick(key)
	s.record(c, "draft."+key+".update")
	httpapi.OK(c, gin.H{"key": key})
}

// setDraftProxy sets the proxy of new-account draft key visible to scope and
// marks it used.
func setDraftProxy(db *gorm.DB, key string, scope, proxyID *int64) *gorm.DB {
	return newAccountDraft(db, key, scope).Updates(map[string]any{"proxy_id": proxyID, "last_seen_at": gorm.Expr("now()")})
}

// draftDelete serves DELETE /system/ccgateway/drafts/:key → 204: the runtime
// is removed from the controller and the row deleted. When the controller
// cannot be reached the draft is marked expired so the sweep retries soon.
func (s *Service) draftDelete(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	key := c.Param("key")
	if ok, e := s.touchDraft(ctx, key, draftScope(ctx)); e != nil || !ok {
		httpapi.Fail(c, errOr(e, draftNotFound(core.ErrNotFound)))
		return
	}
	if _, e := s.removeDraft(ctx, key, true); e != nil {
		openDraft(store.G(context.WithoutCancel(ctx), s.DB.Gorm), key).Update("last_seen_at", gorm.Expr("now() - interval '1 day'"))
		httpapi.Fail(c, transportError(e))
		return
	}
	s.record(c, "draft."+key+".delete")
	httpapi.NoContent(c)
}

// draftAction serves GET status|health|session and POST
// sync|start|complete|cancel on a draft, like the account endpoints.
func (s *Service) draftAction(c *gin.Context) {
	httpapi.NoStore(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	key := c.Param("key")
	if ok, e := s.touchDraft(ctx, key, draftScope(ctx)); e != nil || !ok {
		httpapi.Fail(c, errOr(e, draftNotFound(core.ErrNotFound)))
		return
	}
	d, e := s.draftDesired(ctx, key, false)
	if e != nil {
		httpapi.Fail(c, errOr(onlyUnexpected(e), draftNotFound(core.ErrNotFound)))
		return
	}
	s.serveRuntime(c, ctx, d, true)
}

// errOr is e unless it is nil, then fallback.
func errOr(e error, fallback error) error {
	if e != nil {
		return e
	}
	return fallback
}

// onlyUnexpected drops "no rows" (the draft vanished meanwhile).
func onlyUnexpected(e error) error {
	if store.IsNoRows(e) {
		return nil
	}
	return e
}

// removeDraft deletes an unadopted draft: under the runtime's advisory lock
// (no Reconcile can recreate it meanwhile) the row is deleted, the runtime
// removed on the controller, and only then the deletion committed. wait
// blocks for the lock; otherwise a busy runtime is skipped. removed=false
// with a nil error: busy, adopted or already gone.
func (s *Service) removeDraft(ctx context.Context, key string, wait bool) (removed bool, err error) {
	if !isDraftKey(key) {
		return false, errors.New("not a draft key")
	}
	e := s.lockedTx(ctx, key, wait, func(tx *gorm.DB) error {
		res := openDraft(store.G(ctx, tx), key).Delete(&model.CcgatewayRuntime{})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		if e := s.deleteRemote(ctx, tx, key, false); e != nil {
			return e
		}
		removed = true
		return nil
	})
	return removed, e
}

// lockedTx runs fn in a transaction holding the runtime's advisory lock and
// commits when fn succeeds. Without wait a held lock returns nil at once.
func (s *Service) lockedTx(ctx context.Context, key string, wait bool, fn func(*gorm.DB) error) error {
	q := tryLockSQL
	if wait {
		q = lockSQL
	}
	return s.DB.Tx(ctx, func(tx *gorm.DB) error {
		var locked bool
		if e := store.G(ctx, tx).Raw(q, key).Row().Scan(&locked); e != nil || !locked {
			return e // a lock held elsewhere: nothing ran, nothing to commit
		}
		return fn(tx)
	})
}

// deleteRemote removes a runtime on the controller (DELETE /accounts/<key>,
// idempotent): a draft runtime, or with retired an account-id runtime a
// re-authorization replaced, which the controller only deletes when
// X-CCG-Delete-Account names it again (§49.17); X-CCG-Delete-Data also removes
// its data volume and recovery backup (a draft's volume always goes, and a
// draft has no backup). 404 counts as removed: a controller without the
// endpoint predates draft keys and cannot have created the runtime.
func (s *Service) deleteRemote(ctx context.Context, q *gorm.DB, key string, retired bool) error {
	var header []string
	switch {
	case isDraftKey(key):
	case retired && accountKeyPattern.MatchString(key):
		header = []string{"X-CCG-Delete-Account", key, "X-CCG-Delete-Data", "1"}
	default:
		return errors.New("only draft and retired runtimes are deleted")
	}
	return s.deleteRuntime(ctx, q, key, true, header...)
}

// CheckDraft is the POST /accounts check of "ccgateway_runtime" (CONTRACTS
// §49.10): the draft exists, the caller may see it (scope nil: every draft),
// it is not adopted, not a re-authorization draft, and Claude Code in its
// container is signed in. Failures are 400 with details.reason
// draft_not_found or draft_not_authorized. The facts carry the subscription
// type Claude Code reported (CONTRACTS §59).
func (s *Service) CheckDraft(ctx context.Context, key string, scope *int64) (core.AccountPreparationFacts, error) {
	return s.checkDraftLogin(ctx, key, scope, accountDraft)
}

// checkDraftLogin checks an open draft of the given kind (accountDraft or an
// account id) and that Claude Code is signed in in it.
func (s *Service) checkDraftLogin(ctx context.Context, key string, scope *int64, kind int64) (core.AccountPreparationFacts, error) {
	var facts core.AccountPreparationFacts
	if ok, e := s.touchDraftOf(ctx, key, scope, kind); e != nil || !ok {
		return facts, errOr(e, draftNotFound(core.ErrInvalidArgument))
	}
	notAuthorized := reasonError(core.ErrInvalidArgument, "draft_not_authorized")
	d, e := s.draftDesired(ctx, key, false)
	if store.IsNoRows(e) {
		return facts, draftNotFound(core.ErrInvalidArgument)
	}
	if e != nil {
		return facts, e
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, close, e := s.runtimeRequest(ctx, key, "GET", "admin/status", nil, d.Revision)
	if e != nil {
		return facts, notAuthorized
	}
	defer close()
	defer res.Body.Close()
	var v struct {
		LoggedIn         bool   `json:"logged_in"`
		SubscriptionType string `json:"subscription_type"`
		AccountEmail     string `json:"account_email"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&v) != nil || !v.LoggedIn {
		return facts, notAuthorized
	}
	return observedFacts(v.SubscriptionType, v.AccountEmail), nil
}

// AdoptDraft makes the draft key the runtime of account accountID, inside
// the transaction that inserts the account. A draft adopted or removed
// meanwhile (or not the caller's, or a re-authorization draft) fails with
// 400 draft_not_found. A sweep deleting the draft concurrently holds the
// row: the update waits and then finds nothing.
func AdoptDraft(ctx context.Context, tx *gorm.DB, key string, accountID int64, scope *int64) error {
	res := newAccountDraft(store.G(ctx, tx), key, scope).
		Updates(map[string]any{"account_id": accountID, "adopted_at": gorm.Expr("now()"), "last_seen_at": gorm.Expr("now()")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return draftNotFound(core.ErrInvalidArgument)
	}
	return nil
}
