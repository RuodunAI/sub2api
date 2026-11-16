package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) registerPreparationRoutes(r *httpapi.Router) {
	r.PermScoped("POST", "/account-types/:plugin_key/:type/preparations", s.createPreparation, perm.AccountCreate).Uses(perm.AccountRelay)
	r.PermScoped("GET", "/account-preparations/:key/:action", s.preparationAction, perm.AccountCreate)
	r.PermScoped("POST", "/account-preparations/:key/:action", s.preparationAction, perm.AccountCreate)
	r.PermScoped("DELETE", "/account-preparations/:key", s.cancelPreparation, perm.AccountCreate)
	r.PermScoped("PUT", "/account-preparations/:key", s.updatePreparation, perm.AccountCreate)
}

func (s *Service) preparationResource(p core.AccountPreparation) (core.AccountLifecycleResource, error) {
	resource := s.d.LifecycleResources[p.PluginKey]
	if resource == nil {
		return nil, core.ErrUnavailable.WithMessage("Account preparation resource is unavailable")
	}
	return resource, nil
}

func (s *Service) preparationCallback(ctx context.Context, p core.AccountPreparation, phase, action string) error {
	binding, ok := s.accountType(p.PluginKey, p.Type)
	if !ok || binding.Type.Lifecycle == nil || !binding.Type.Lifecycle.Preparation {
		return core.ErrPluginUnavailable
	}
	result, err := s.accountLifecycle(ctx, binding, &pluginv1.AccountLifecycleRequest{
		Phase: phase, Action: action, PreparationKey: p.Key,
		Caller:  &pluginv1.Caller{UserId: p.CreatedBy},
		Account: &pluginv1.Account{Type: p.Type, SettingsJson: string(p.Settings), Properties: &pluginv1.AccountProperties{ProxyId: p.ProxyID}},
	})
	if err != nil {
		return err
	}
	if result == nil || !result.RunResource {
		return core.ErrUnavailable.WithMessage("Plugin did not approve account preparation")
	}
	return nil
}

func (s *Service) createPreparation(c *gin.Context) {
	ctx := audit.Context(c)
	binding, ok := s.accountType(c.Param("plugin_key"), c.Param("type"))
	if !ok || binding.Type.Lifecycle == nil || !binding.Type.Lifecycle.Preparation {
		httpapi.Fail(c, core.ErrUnsupported)
		return
	}
	if err := s.checkCustomUpstream(ctx, s.scopeOf(ctx, perm.AccountCreate), binding); err != nil {
		httpapi.Fail(c, err)
		return
	}
	var in struct {
		ProxyID        *int64          `json:"proxy_id"`
		Settings       json.RawMessage `json:"settings"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 65536)
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if err := checkRefs(ctx, s.d.DB.Gorm, in.ProxyID, s.proxyRefs(ctx, perm.AccountCreate)); err != nil {
		httpapi.Fail(c, err)
		return
	}
	if len(in.IdempotencyKey) > 128 {
		httpapi.Fail(c, core.ErrInvalidArgument)
		return
	}
	settings, err := s.mergeExtensions(ctx, binding, []byte("{}"), in.Settings)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		httpapi.Fail(c, err)
		return
	}
	uid, _ := core.UserID(ctx)
	p := core.AccountPreparation{Key: "d" + hex.EncodeToString(random[:]), PluginKey: binding.Plugin.Key, Type: binding.Type.ID, ProxyID: in.ProxyID, Settings: settings, CreatedBy: uid}
	resource, err := s.preparationResource(p)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	// A preparation takes its resource (a runtime container) until it is
	// adopted, cancelled or expires; the full account:create is not limited.
	limit := core.MaxOpenDrafts
	if s.scopeOf(ctx, perm.AccountCreate).All {
		limit = 0 // no limit
	}
	var fresh bool // this request inserted the row
	err = s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		var matches, active bool
		// The count and the insert run under the user's lock. A repeated
		// idempotency key is not a new preparation and passes.
		if limit > 0 {
			if err := store.XactLock(ctx, tx, core.OpenDraftsLock(uid)); err != nil {
				return err
			}
		}
		// The arguments repeat where the statement reuses one ($n on pgx).
		err := store.G(ctx, tx).Raw(`INSERT INTO account_preparations(key,plugin_key,account_type,proxy_id,settings,created_by,idempotency_key)
			SELECT ?,?::text,?::text,?,?::jsonb,?::bigint,NULLIF(?::text,'')
			WHERE ?::int = 0
				OR (SELECT count(*) FROM account_preparations WHERE created_by=?::bigint AND account_id IS NULL AND state IN ('pending','ready') AND expires_at>now()) < ?::int
				OR EXISTS (SELECT 1 FROM account_preparations WHERE created_by=?::bigint AND plugin_key=?::text AND account_type=?::text AND idempotency_key=NULLIF(?::text,''))
			ON CONFLICT (created_by,plugin_key,account_type,idempotency_key) DO UPDATE SET last_seen_at=now()
			RETURNING key,settings=?::jsonb AND proxy_id IS NOT DISTINCT FROM ?::bigint,account_id IS NULL AND state IN ('pending','ready') AND expires_at>now(),xmax=0`,
			p.Key, p.PluginKey, p.Type, p.ProxyID, p.Settings, uid, in.IdempotencyKey,
			limit,
			uid, limit,
			uid, p.PluginKey, p.Type, in.IdempotencyKey,
			p.Settings, p.ProxyID).Row().Scan(&p.Key, &matches, &active, &fresh)
		if store.IsNoRows(err) {
			return core.ErrTooManyDrafts
		}
		if err != nil {
			return err
		}
		if !matches || !active {
			return core.ErrConflict.WithMessage("Preparation key was already used with another configuration or is no longer available")
		}
		return audit.Audit(ctx, tx, uid, "account.prepare", "account_preparation", p.Key, map[string]any{"plugin_key": p.PluginKey, "type": p.Type})
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	ctx, release, err := s.lockPreparation(ctx, p.Key)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	defer release()
	if loaded, e := s.loadPreparation(ctx, p.Key); e != nil {
		err = e
	} else {
		p = loaded
		err = s.preparationCallback(ctx, p, "prepare", "")
	}
	if err == nil {
		err = resource.PrepareAccount(ctx, p)
	}
	if err != nil {
		if fresh {
			// The key was never returned, so nobody could cancel it and it
			// would hold a core.MaxOpenDrafts slot until the sweep.
			abandon, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if err := s.disposePreparation(abandon, p, disposeAbandoned); err != nil {
				slog.WarnContext(abandon, "account: abandon failed preparation", "key", p.Key, "err", err)
			}
			cancel()
		}
		httpapi.Fail(c, err)
		return
	}
	httpapi.Created(c, gin.H{"key": p.Key})
}

func (s *Service) loadPreparation(ctx context.Context, key string) (core.AccountPreparation, error) {
	var p model.AccountPreparation
	uid, _ := core.UserID(ctx)
	res := touchPreparation(store.G(ctx, s.d.DB.Gorm), &p, key, uid)
	if res.Error == nil && res.RowsAffected == 0 {
		return core.AccountPreparation{}, core.ErrNotFound
	}
	return preparationOf(p), res.Error
}

// preparationColumns are the columns of core.AccountPreparation.
const preparationColumns = "key,plugin_key,account_type,proxy_id,settings,created_by"

func preparationOf(p model.AccountPreparation) core.AccountPreparation {
	return core.AccountPreparation{Key: p.Key, PluginKey: p.PluginKey, Type: p.AccountType, ProxyID: p.ProxyID,
		Settings: p.Settings, CreatedBy: p.CreatedBy}
}

// openPreparation is preparation key of uid while it is open: not adopted,
// pending or ready, and not expired.
func openPreparation(db *gorm.DB, key string, uid int64) *gorm.DB {
	return db.Where("key = ? AND created_by = ? AND account_id IS NULL AND state IN ('pending','ready') AND expires_at > now()", key, uid)
}

// touchPreparation marks open preparation key of uid seen, reading it into
// into.
func touchPreparation(db *gorm.DB, into *model.AccountPreparation, key string, uid int64) *gorm.DB {
	return openPreparation(db.Model(into), key, uid).
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "key"}, {Name: "plugin_key"}, {Name: "account_type"}, {Name: "proxy_id"}, {Name: "settings"}, {Name: "created_by"}}}).
		Update("last_seen_at", gorm.Expr("now()"))
}

func (s *Service) preparationAction(c *gin.Context) {
	ctx, cancel := context.WithTimeout(audit.Context(c), 90*time.Second)
	defer cancel()
	action := c.Param("action")
	read := slices.Contains([]string{"status", "health", "session"}, action)
	if (!read && !slices.Contains([]string{"sync", "start", "complete", "cancel"}, action)) || (c.Request.Method == "GET") != read {
		httpapi.Fail(c, core.ErrNotFound)
		return
	}
	p, err := s.loadPreparation(ctx, c.Param("key"))
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	ctx, release, err := s.lockPreparation(ctx, p.Key)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	defer release()
	payload := json.RawMessage("{}")
	if !read {
		payload, err = io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
		var object map[string]json.RawMessage
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		if err != nil || json.Unmarshal(payload, &object) != nil || object == nil {
			httpapi.Fail(c, core.ErrInvalidArgument)
			return
		}
	}
	if err := s.preparationCallback(ctx, p, "action", action); err != nil {
		httpapi.Fail(c, err)
		return
	}
	resource, err := s.preparationResource(p)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	result, err := resource.AccountPreparationAction(ctx, p, c.Request.Method, action, payload)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if !read {
		uid, _ := core.UserID(ctx)
		if err := audit.Audit(ctx, s.d.DB.Gorm, uid, "account.preparation."+action, "account_preparation", p.Key, nil); err != nil {
			httpapi.Fail(c, err)
			return
		}
	}
	httpapi.OK(c, result)
}

func (s *Service) updatePreparation(c *gin.Context) {
	ctx := audit.Context(c)
	p, err := s.loadPreparation(ctx, c.Param("key"))
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	ctx, release, err := s.lockPreparation(ctx, p.Key)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	defer release()
	var in struct {
		ProxyID *int64 `json:"proxy_id"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if err := checkRefs(ctx, s.d.DB.Gorm, in.ProxyID, s.proxyRefs(ctx, perm.AccountCreate)); err != nil {
		httpapi.Fail(c, err)
		return
	}
	p.ProxyID = in.ProxyID
	if err := s.preparationCallback(ctx, p, "prepare", ""); err != nil {
		httpapi.Fail(c, err)
		return
	}
	resource, err := s.preparationResource(p)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	err = s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		res := openPreparation(store.G(ctx, tx).Model(&model.AccountPreparation{}), p.Key, p.CreatedBy).
			Updates(map[string]any{"proxy_id": p.ProxyID, "last_seen_at": gorm.Expr("now()")})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return core.ErrConflict
		}
		if err := resource.UpdateAccountPreparation(ctx, tx, p); err != nil {
			return err
		}
		return audit.Audit(ctx, tx, p.CreatedBy, "account.preparation.update", "account_preparation", p.Key, map[string]any{"fields": []string{"proxy_id"}})
	})
	if err == nil {
		_, err = resource.AccountPreparationAction(ctx, p, "POST", "sync", json.RawMessage("{}"))
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, gin.H{"key": p.Key})
}

func (s *Service) cancelPreparation(c *gin.Context) {
	ctx := audit.Context(c)
	uid, _ := core.UserID(ctx)
	var p model.AccountPreparation
	err := store.G(ctx, s.d.DB.Gorm).Select(preparationColumns).
		Where("key = ? AND created_by = ? AND account_id IS NULL AND state IN ('pending','ready','cancelling')", c.Param("key"), uid).
		Take(&p).Error
	if store.IsNoRows(err) {
		err = core.ErrNotFound
	}
	if err == nil {
		err = s.disposePreparation(ctx, preparationOf(p), disposeCancelled)
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.NoContent(c)
}

func (s *Service) lockPreparation(ctx context.Context, key string) (context.Context, func(), error) {
	if s.d.Locker == nil {
		return ctx, func() {}, nil
	}
	lock, ok, err := s.d.Locker.TryLock(ctx, "account:preparation:"+key, 2*time.Minute)
	if err != nil {
		return ctx, nil, err
	}
	if !ok {
		return ctx, nil, core.ErrConflict.WithMessage("Account preparation is busy; retry shortly")
	}
	ctx, cancel := core.KeepLock(ctx, lock)
	return ctx, func() { cancel(); lock.Release() }, nil
}

// Why a preparation is disposed of (disposePreparation).
const (
	// disposeCancelled: its creator cancels it (DELETE
	// /account-preparations/:key); the plugin may refuse.
	disposeCancelled = "cancelled"
	// disposeAbandoned: its first prepare failed; the caller holds its lock.
	// The row goes, giving back the core.MaxOpenDrafts slot and the
	// idempotency key.
	disposeAbandoned = "abandoned"
	// disposeExpired: the sweep, for one that expired, was left alone or is
	// still being cancelled.
	disposeExpired = "expired"
)

// disposePreparation cancels p for reason: it marks the row cancelling,
// which no longer counts as open, tells the plugin (the cancel phase: before
// that for a cancel by its creator, which a refusal stops; otherwise once
// the row starts cancelling), has the resource drop what it started, then marks it cancelled (an abandoned one is deleted) and audits
// it. A row not in a state reason applies to is a conflict, but for the
// sweep. A failure leaves it cancelling for the sweep to retry.
func (s *Service) disposePreparation(ctx context.Context, p core.AccountPreparation, reason string) error {
	if reason != disposeAbandoned {
		locked, release, err := s.lockPreparation(ctx, p.Key)
		if err != nil {
			return err
		}
		defer release()
		ctx = locked
	}
	resource, err := s.preparationResource(p)
	if err != nil {
		return err
	}
	if reason == disposeCancelled {
		if err := s.preparationCallback(ctx, p, "cancel", ""); err != nil {
			return err // the plugin may refuse its creator's cancel
		}
	}
	var was string
	g := store.G(ctx, s.d.DB.Gorm)
	err = g.Raw(`UPDATE account_preparations ap SET state='cancelling'
		FROM (SELECT key, state FROM account_preparations WHERE key=? FOR UPDATE) old
		WHERE ap.key=old.key AND ap.account_id IS NULL
		AND CASE ?::text WHEN 'abandoned' THEN ap.state='pending'
			WHEN 'expired' THEN ap.state='cancelling' OR ap.state IN ('pending','ready') AND (ap.expires_at<=now() OR ap.last_seen_at<now()-interval '15 minutes')
			ELSE ap.state IN ('pending','ready','cancelling') END
		RETURNING old.state`, p.Key, reason).Row().Scan(&was)
	if store.IsNoRows(err) {
		if reason == disposeExpired {
			return nil
		}
		return core.ErrConflict
	}
	if err != nil {
		return err
	}
	// The system disposing of it tells the plugin once, when it starts to
	// (not on a retry of one already cancelling); a refusal does not stop it.
	if reason != disposeCancelled && was != "cancelling" {
		if err := s.preparationCallback(ctx, p, "cancel", ""); err != nil {
			slog.WarnContext(ctx, "account: preparation cancel callback", "key", p.Key, "reason", reason, "err", err)
		}
	}
	if err := resource.CancelAccountPreparation(ctx, p); err != nil {
		return err
	}
	final := g.Where("key = ? AND account_id IS NULL AND state = 'cancelling'", p.Key)
	if reason == disposeAbandoned {
		final = final.Delete(&model.AccountPreparation{})
	} else {
		final = final.Model(&model.AccountPreparation{}).Update("state", "cancelled")
	}
	if err := final.Error; err != nil {
		return err
	}
	actor := p.CreatedBy
	if reason == disposeExpired {
		actor = 0
	}
	return audit.Audit(ctx, s.d.DB.Gorm, actor, "account.preparation.cancel", "account_preparation", p.Key, map[string]any{"reason": reason})
}

// checkPreparationForCreate checks the preparation_key of POST /accounts and
// returns what the preparation observed of the account (CONTRACTS §59).
func (s *Service) checkPreparationForCreate(ctx context.Context, bt core.AccountTypeBinding, in *input, settings []byte) (*core.AccountPreparation, core.AccountPreparationFacts, error) {
	var none core.AccountPreparationFacts
	if in.PreparationKey == nil {
		if bt.Type.Lifecycle != nil && bt.Type.Lifecycle.Required && in.CCGatewayRuntime == nil {
			return nil, none, core.ErrInvalidArgument.WithMessage("Account preparation is required")
		}
		return nil, none, nil
	}
	if in.CCGatewayRuntime != nil || in.ProxyURL != nil {
		return nil, none, core.ErrInvalidArgument.WithMessage("Preparation cannot be combined with another runtime or proxy URL")
	}
	p, err := s.loadPreparation(ctx, *in.PreparationKey)
	if err != nil {
		return nil, none, err
	}
	proxyMatches := p.ProxyID == nil && !in.ProxyID.Valid || p.ProxyID != nil && in.ProxyID.Valid && *p.ProxyID == in.ProxyID.V
	if p.PluginKey != bt.Plugin.Key || p.Type != bt.Type.ID || !proxyMatches {
		return nil, none, core.ErrInvalidArgument.WithMessage("Preparation does not match this account")
	}
	var equal bool
	if err := store.G(ctx, s.d.DB.Gorm).Raw(`SELECT ?::jsonb=?::jsonb`, p.Settings, settings).Row().Scan(&equal); err != nil {
		return nil, none, err
	}
	if !equal {
		return nil, none, core.ErrConflict.WithMessage("Preparation settings changed; prepare a new runtime first")
	}
	if err := s.preparationCallback(ctx, p, "check", ""); err != nil {
		return nil, none, err
	}
	resource, err := s.preparationResource(p)
	if err != nil {
		return nil, none, err
	}
	facts, err := resource.CheckAccountPreparation(ctx, p)
	if err != nil {
		return nil, none, err
	}
	return &p, facts, nil
}

func (s *Service) adoptPreparation(ctx context.Context, tx *gorm.DB, p core.AccountPreparation, id int64) error {
	res := openPreparation(store.G(ctx, tx).Model(&model.AccountPreparation{}), p.Key, p.CreatedBy).
		Where("proxy_id IS NOT DISTINCT FROM ?::bigint AND settings = ?::jsonb", p.ProxyID, p.Settings).
		Updates(map[string]any{"state": "adopted", "account_id": id})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return core.ErrConflict.WithMessage("Account preparation is no longer available")
	}
	resource, err := s.preparationResource(p)
	if err != nil {
		return err
	}
	return resource.AdoptAccountPreparation(ctx, tx, p, id)
}
