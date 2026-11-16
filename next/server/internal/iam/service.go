// Package iam implements console identity: users, login with sessions
// kept in Valkey (s2s_, CONTRACTS §100), and the super admin
// bootstrap. It implements core.TokenVerifier.
package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/config"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/secret"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// statusCacheTTL bounds how long a disabled user's session keeps working
// on a node that missed the authz:changed or iam:user:changed broadcast.
const statusCacheTTL = 30 * time.Second

// Deps are the dependencies of Service.
type Deps struct {
	DB     *store.DB
	Redis  redis.Cmdable
	Config *config.Config
	Events core.EventPublisher
	Authz  *authz.Service
	// Bus announces ended sessions to the other nodes (iam:user:changed).
	// Optional: without it only this node's status cache is dropped.
	Bus core.Bus
	// Cipher seals the two-factor secrets.
	Cipher *secret.Cipher
	// BcryptCost defaults to bcrypt.DefaultCost (tests lower it).
	BcryptCost int
	// PriceRulesChanged drops the cached price rules on every node
	// (billing.Service.PriceRulesChanged): deleting a user deletes those of
	// their personal segment. Optional.
	PriceRulesChanged func(ctx context.Context)
	// SessionRedis keeps the sessions and the flows (tokenstore.NewClient
	// on Redis's server); Redis when nil.
	SessionRedis redis.Cmdable
}

// Service is the iam module.
type Service struct {
	db     *store.DB
	rdb    redis.Cmdable
	cfg    *config.Config
	events core.EventPublisher
	authz  *authz.Service
	bus    core.Bus
	cipher *secret.Cipher
	cost   int

	priceRulesChanged func(ctx context.Context)

	limiter *loginLimiter

	// sessions and flows are in Valkey (nil without Redis: the server
	// CLI, which signs nobody in); sessionIdle is the idle timeout of a
	// session.
	sessions    tokenstore.SessionStore
	flows       tokenstore.FlowStore
	sessionIdle time.Duration

	dummyHash []byte

	mu          sync.Mutex
	status      map[int64]statusEntry
	statusEpoch uint64
}

type statusEntry struct {
	active       bool
	tokenVersion int
	at           time.Time
}

var (
	_ core.TokenVerifier     = (*Service)(nil)
	_ core.AccessKeyVerifier = (*Service)(nil)
	_ core.StepUpVerifier    = (*Service)(nil)
	_ core.StepUpSessions    = (*Service)(nil)
)

// New creates the service. The authz service must be started before
// Bootstrap is called (built-in roles are seeded by authz.Start).
func New(d Deps) *Service {
	if d.BcryptCost == 0 {
		d.BcryptCost = bcrypt.DefaultCost
	}
	if d.PriceRulesChanged == nil {
		d.PriceRulesChanged = func(context.Context) {}
	}
	s := &Service{db: d.DB, rdb: d.Redis, cfg: d.Config, events: d.Events, authz: d.Authz, bus: d.Bus, cipher: d.Cipher, cost: d.BcryptCost, status: map[int64]statusEntry{},
		priceRulesChanged: d.PriceRulesChanged, limiter: newLoginLimiter(d.Redis)}
	s.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), s.cost)
	if d.Redis != nil {
		var o tokenstore.Options
		if d.Config != nil {
			o.Idle, o.Max = d.Config.SessionIdleTTL, d.Config.SessionMaxTTL
		}
		rdb := d.SessionRedis
		if rdb == nil {
			rdb = d.Redis
		}
		v := tokenstore.NewValkey(rdb, o)
		s.sessions, s.flows, s.sessionIdle = v, v, v.Idle()
	}
	if d.Authz != nil {
		d.Authz.UseStepUp(s)
		// Status changes (disable/delete) bump the authz version.
		d.Authz.OnChange(s.clearStatusCache)
	}
	if d.Bus != nil {
		s.subscribe(d.Bus)
	}
	return s
}

// subscribe keeps the status cache in step with sessions ended on other
// nodes; after a reconnect, whose missed messages are lost, it drops it all.
func (s *Service) subscribe(bus core.Bus) {
	bus.Subscribe(core.ChannelUserSessionsChanged, func(payload []byte) {
		uid, err := strconv.ParseInt(string(payload), 10, 64)
		if err != nil {
			s.clearStatusCache()
			return
		}
		s.forgetStatus(uid)
	})
	if r, ok := bus.(core.BusReconnects); ok {
		r.OnReconnect(s.clearStatusCache)
	}
}

func (s *Service) clearStatusCache() {
	s.mu.Lock()
	s.statusEpoch++
	s.status = map[int64]statusEntry{}
	s.mu.Unlock()
}

func (s *Service) forgetStatus(userID int64) {
	s.mu.Lock()
	s.statusEpoch++
	delete(s.status, userID)
	s.mu.Unlock()
}

// ------------------------------------------------------------ sessions

// VerifySession implements core.TokenVerifier: the user and the session
// id of an s2s_ token. Valkey down fails with 503 (tokenstore.ErrUnavailable);
// there is no fallback. A session from before a token_version bump that did
// not reach Valkey (ending every session, the server CLI) is dropped on
// sight; a token a rotation left in its grace drops only itself, never the
// session's current token.
//
// The user's status is read through the cache whatever the session's
// stamp: a stamp ahead of the row (a survivor of a bump not committed yet,
// or rolled back) is accepted, and a stale cache can only be behind a newer
// session, which it accepts too.
func (s *Service) VerifySession(ctx context.Context, token string) (int64, string, error) {
	if s.sessions == nil || !strings.HasPrefix(token, tokenstore.SessionPrefix) {
		return 0, "", core.ErrUnauthenticated
	}
	sess, err := s.sessions.Lookup(ctx, token)
	if errors.Is(err, tokenstore.ErrNotFound) {
		return 0, "", core.ErrUnauthenticated.WithMessage("session has ended")
	}
	if err != nil {
		return 0, "", err
	}
	active, version, err := s.isActiveWithVersion(ctx, sess.UserID)
	if err != nil {
		return 0, "", err
	}
	if !active || sess.TokenVersion < version {
		if err := s.sessions.Drop(ctx, token, sess); err != nil {
			return 0, "", err
		}
		return 0, "", core.ErrUnauthenticated.WithMessage("session has been invalidated")
	}
	if err := s.sessions.Touch(ctx, token, sess); err != nil {
		return 0, "", err
	}
	return sess.UserID, sess.ID, nil
}

// isActiveWithVersion reads the user's status through the cache. A deleted
// user is cached with the highest version, since no session can be stamped
// newer than that: their leftover sessions do not read the row on every
// request.
func (s *Service) isActiveWithVersion(ctx context.Context, uid int64) (bool, int, error) {
	s.mu.Lock()
	e, ok := s.status[uid]
	epoch := s.statusEpoch
	s.mu.Unlock()
	if ok && time.Since(e.at) < statusCacheTTL {
		return e.active, e.tokenVersion, nil
	}
	var active bool
	var version int
	err := userStatus(store.G(ctx, s.db.Gorm), uid).Row().Scan(&active, &version)
	if store.IsNoRows(err) {
		active, version, err = false, math.MaxInt, nil
	}
	if err != nil {
		return false, 0, err
	}
	s.mu.Lock()
	if epoch == s.statusEpoch {
		s.status[uid] = statusEntry{active: active, tokenVersion: version, at: time.Now()}
	}
	s.mu.Unlock()
	return active, version, nil
}

// userStatus is the statement of isActiveWithVersion, raw (the fewest
// allocations): a request reads it on a cache miss.
func userStatus(db *gorm.DB, uid int64) *gorm.DB {
	return db.Raw(`SELECT status = 'active', token_version FROM users WHERE id = ? AND deleted_at IS NULL`, uid)
}

// ------------------------------------------------------------ bootstrap

// bootstrapLock serializes the bootstraps of the nodes.
func bootstrapLock(db *gorm.DB) *gorm.DB {
	return db.Exec(`SELECT pg_advisory_xact_lock(hashtext('sub2api:iam:bootstrap'))`)
}

// liveUsers are the users not deleted.
func liveUsers(db *gorm.DB) *gorm.DB {
	return db.Model(&model.User{}).Where("deleted_at IS NULL")
}

// Bootstrap creates the first super admin from config when the database has
// no users. Without configured credentials it only logs a warning.
func (s *Service) Bootstrap(ctx context.Context) error {
	email, password := s.cfg.BootstrapAdminEmail, s.cfg.BootstrapAdminPassword
	var created *User
	var version int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := bootstrapLock(g).Error; err != nil {
			return err
		}
		exists, err := store.Exists(g, liveUsers(g))
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		if email == "" || password == "" {
			slog.WarnContext(ctx, "iam: no users exist and SUB2API_BOOTSTRAP_ADMIN_EMAIL/PASSWORD are not set")
			return nil
		}
		u, v, err := s.createUserTx(ctx, tx, 0, CreateUserInput{
			Email: email, DisplayName: "Administrator", Password: password,
			RoleKeys: []string{authz.RoleSuperAdmin},
		})
		if err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
		created, version = u, v
		return nil
	})
	if err != nil {
		return err
	}
	if created != nil {
		s.authz.Committed(ctx, version)
		slog.InfoContext(ctx, "iam: bootstrap super admin created", "email", created.Email, "user_id", created.ID)
	}
	return nil
}
