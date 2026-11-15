package account

import (
	"cmp"
	"context"
	"maps"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var _ core.AccountAutomaticStateController = (*Service)(nil)

// automaticStateSweepInterval paces the write-back of expired timed disables;
// routing honours the release time on its own (Candidates).
const automaticStateSweepInterval = time.Minute

// usageLimitsInterval is core.account.usage_limits's period.
const usageLimitsInterval = 5 * time.Second
const maxAutomaticDisableDuration = 366 * 24 * time.Hour

func automaticDisableEnabled(ctx context.Context, q *gorm.DB) (bool, error) {
	var enabled bool
	err := store.G(ctx, q).Raw(`SELECT COALESCE(
		(SELECT (value->>'account_enabled')::boolean FROM settings WHERE key='failure_policy'),
		(SELECT (value->>'enabled')::boolean FROM settings WHERE key='auto_disable'),true)`).Row().Scan(&enabled)
	return enabled, err
}

func (s *Service) getAutoDisablePolicy(c *gin.Context) {
	enabled, err := automaticDisableEnabled(c.Request.Context(), s.d.DB.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, gin.H{"enabled": enabled})
}

func (s *Service) ApplyAccountAutomaticState(ctx context.Context, id int64, state *pluginv1.AccountAutomaticState) (*pluginv1.SetAccountAutomaticStateResponse, error) {
	return s.applyAutomaticState(ctx, "", id, state, false)
}

func (s *Service) ApplyPluginAccountAutomaticState(ctx context.Context, pluginKey string, id int64, state *pluginv1.AccountAutomaticState) (*pluginv1.SetAccountAutomaticStateResponse, error) {
	if pluginKey == "" {
		return nil, core.ErrInvalidArgument.WithMessage("plugin key is required")
	}
	return s.applyAutomaticState(ctx, pluginKey, id, state, false)
}

func (s *Service) applyAutomaticState(ctx context.Context, owner string, id int64, state *pluginv1.AccountAutomaticState, policyAuthorized bool) (*pluginv1.SetAccountAutomaticStateResponse, error) {
	if id <= 0 || state == nil || !utf8.ValidString(state.GetReason()) || len(state.GetReason()) > 1000 {
		return nil, core.ErrInvalidArgument.WithMessage("invalid automatic account state")
	}
	if state.GetAction() != pluginv1.AccountAutomaticState_ACTION_DISABLE && state.GetAction() != pluginv1.AccountAutomaticState_ACTION_CLEAR {
		return nil, core.ErrInvalidArgument.WithMessage("automatic state action must be disable or clear")
	}
	if state.GetAction() == pluginv1.AccountAutomaticState_ACTION_CLEAR && state.GetRelease() != nil {
		return nil, core.ErrInvalidArgument.WithMessage("clear cannot specify a release time")
	}
	out := &pluginv1.SetAccountAutomaticStateResponse{}
	var name, pluginKey, typ string
	var cooldownCleared bool
	err := s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		var allowed bool
		var until, deferredUntil *time.Time
		var now time.Time
		var source, deferredReason string
		var deferred bool
		err := lockAutomaticState(store.G(ctx, tx), id, owner).Row().
			Scan(&name, &pluginKey, &typ, &out.Status, &out.Reason, &source, &allowed, &until, &deferred, &deferredReason, &deferredUntil, &now)
		if store.IsNoRows(err) {
			return notFound(ctx)
		}
		if err != nil {
			return err
		}
		// A clear confirms the account is usable now, so its runtime
		// cooldown goes whatever the status; the branches below still keep
		// an administrator's, an error's and the usage limit's block
		// (CONTRACTS §91).
		if state.GetAction() == pluginv1.AccountAutomaticState_ACTION_CLEAR {
			if cooldownCleared, err = s.clearCooldown(ctx, id); err != nil {
				return err
			}
		}
		var release *time.Time
		switch r := state.GetRelease().(type) {
		case *pluginv1.AccountAutomaticState_UntilUnix:
			t := time.Unix(r.UntilUnix, 0).UTC()
			if !t.After(now) || t.After(now.Add(maxAutomaticDisableDuration)) {
				return core.ErrInvalidArgument.WithMessage("release time must be in the next 366 days")
			}
			release = &t
		case *pluginv1.AccountAutomaticState_DurationSeconds:
			if r.DurationSeconds <= 0 || r.DurationSeconds > int64(maxAutomaticDisableDuration/time.Second) {
				return core.ErrInvalidArgument.WithMessage("disable duration must be between 1 second and 366 days")
			}
			t := now.Add(time.Duration(r.DurationSeconds) * time.Second)
			release = &t
		}
		if until != nil {
			out.UntilUnix = until.Unix()
		}
		if out.Status == "disabled" || (owner != "" && state.GetAction() == pluginv1.AccountAutomaticState_ACTION_DISABLE) {
			return nil
		}
		previous, limited := out.Status, limitDisabled(out.Status, source)
		next, reason := "auto_disabled", state.GetReason()
		viaPlugin := map[string]any{"via_plugin": owner != ""}
		if state.GetAction() == pluginv1.AccountAutomaticState_ACTION_CLEAR {
			// The usage limit's own disable is lifted by the limit or an
			// administrator only (CONTRACTS §73.4); a clear drops the
			// disable the limit held back for when it lets go.
			if limited {
				if !deferred {
					return nil
				}
				return writeDeferredDisable(ctx, tx, id, false, "", nil, viaPlugin)
			}
			if out.Status != "auto_disabled" {
				return nil
			}
			next, reason = "active", ""
		} else {
			globallyAllowed := policyAuthorized && owner == ""
			if !globallyAllowed {
				globallyAllowed, err = automaticDisableEnabled(ctx, tx)
				if err != nil {
					return err
				}
			}
			if !allowed || !globallyAllowed {
				return nil
			}
			if limited {
				// Another disable only replaces the usage limit's when it
				// lasts longer; the limit applies again once that one ends.
				// A shorter one is held back: if the limit lets the account
				// go first (raised, turned off), it passes to that disable
				// for the rest of its time (§73.3).
				if until == nil || release != nil && !release.After(*until) {
					running := deferred && (deferredUntil == nil || deferredUntil.After(now))
					if running && deferredUntil == nil && release != nil ||
						running && deferredReason == reason && sameTime(deferredUntil, release) {
						return nil
					}
					return writeDeferredDisable(ctx, tx, id, true, reason, release, viaPlugin)
				}
			} else if out.Status == "auto_disabled" && until == nil && release != nil {
				return nil
			}
		}
		sameRelease := until == nil && release == nil || until != nil && release != nil && until.Equal(*release)
		if out.Status == next && out.Reason == reason && sameRelease {
			return nil
		}
		out.Changed, out.Status, out.Reason, out.UntilUnix = true, next, reason, 0
		if release != nil {
			out.UntilUnix = release.Unix()
		}
		return s.writeAutomaticStatus(ctx, tx, "account.automatic_state", automaticChange{id: id, name: name, pluginKey: pluginKey, typ: typ,
			from: previous, to: next, reason: reason, until: release, audit: viaPlugin})
	})
	if err != nil {
		return nil, err
	}
	out.Changed = out.Changed || cooldownCleared
	if out.Changed {
		s.afterStatusChange(ctx, id, pluginKey, typ)
	}
	return out, nil
}

// writeDeferredDisable records (set) or drops the disable of a plugin or
// failure policy that the usage limit holds back, on an account the caller
// locked in tx; the status stays the limit's. The 0070 trigger drops it once
// the account is no longer the limit's.
func writeDeferredDisable(ctx context.Context, tx *gorm.DB, id int64, set bool, reason string, until *time.Time, detail map[string]any) error {
	if err := deferDisable(store.G(ctx, tx), id, set, reason, until).Error; err != nil {
		return err
	}
	d := map[string]any{"deferred_by_usage_limit": set, "until": until}
	maps.Copy(d, detail)
	return audit.Audit(ctx, tx, 0, "account.automatic_state", "account", itoa(id), d)
}

// deferDisable records or drops the held-back disable of account id,
// leaving updated_at alone.
func deferDisable(db *gorm.DB, id int64, set bool, reason string, until *time.Time) *gorm.DB {
	return db.Model(&model.Account{}).Where("id = ?", id).Updates(map[string]any{
		"deferred_auto_disable": set, "deferred_auto_disable_reason": reason, "deferred_auto_disabled_until": until,
	})
}

// automaticChange is one automatic status change of an account.
type automaticChange struct {
	id                   int64
	name, pluginKey, typ string
	// from is the status the account had.
	from       string
	to, reason string
	until      *time.Time
	// source is the auto_disabled_source written with the change:
	// usageLimitSource for the usage limit's disable, '' otherwise.
	source string
	// eventReason is the account.status_changed reason, reason by default.
	eventReason string
	// audit adds to the audit detail.
	audit map[string]any
}

// writeAutomaticStatus writes an automatic change of an account the caller
// locked in tx, with its audit row and account.status_changed event; the
// caller runs afterStatusChange once tx commits. It declares that tx writes
// auto_disabled_source: the 0070 trigger otherwise takes a reason or release
// time change of an automatic disable for a writer that does not know the
// source (an older core) and clears the mark.
func (s *Service) writeAutomaticStatus(ctx context.Context, tx *gorm.DB, action string, ch automaticChange) error {
	g := store.G(ctx, tx)
	if err := g.Exec(`SELECT set_config('sup2api.auto_disabled_source', 'written', true)`).Error; err != nil {
		return err
	}
	if err := ch.write(g).Error; err != nil {
		return err
	}
	detail := map[string]any{"plugin_key": ch.pluginKey, "previous_status": ch.from, "status": ch.to, "until": ch.until}
	maps.Copy(detail, ch.audit)
	if err := audit.Audit(ctx, tx, 0, action, "account", itoa(ch.id), detail); err != nil {
		return err
	}
	payload := statusPayload(ch.id, ch.pluginKey, ch.typ, ch.name, ch.to, cmp.Or(ch.eventReason, ch.reason), nil)
	if ch.until != nil {
		payload["auto_disabled_until"] = ch.until.UTC().Format(time.RFC3339)
	}
	return s.d.Events.Emit(ctx, tx, core.Event{Type: core.EventAccountStatusChanged, Payload: payload})
}

// write updates the account to the change, moving updated_at.
func (ch automaticChange) write(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Account{}).Where("id = ?", ch.id).Updates(map[string]any{"status": ch.to, "status_reason": ch.reason,
		"auto_disabled_until": ch.until, "auto_disabled_source": ch.source, "updated_at": gorm.Expr("clock_timestamp()")})
}

// lockAutomaticState locks live account id (of owner's plugin, any for "")
// and reads what an automatic state change decides on, with the time.
func lockAutomaticState(db *gorm.DB, id int64, owner string) *gorm.DB {
	return db.Model(&model.Account{}).Select(`name,plugin_key,type,status,status_reason,auto_disabled_source,auto_disable,auto_disabled_until,
		deferred_auto_disable,deferred_auto_disable_reason,deferred_auto_disabled_until,clock_timestamp()`).
		Where("id = ? AND deleted_at IS NULL AND (? = '' OR plugin_key = ?)", id, owner, owner).
		Clauses(store.ForUpdate)
}

// afterStatusChange tells the routing cache and the CCGateway runtime of a
// committed status change.
func (s *Service) afterStatusChange(ctx context.Context, id int64, pluginKey, typ string) {
	s.changed(ctx, id)
	s.kickCCGateway(id, pluginKey, typ)
}

func (s *Service) SweepAutomaticState(ctx context.Context) (int, error) {
	var accounts []recoveredAccount
	err := s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		if err := recoverExpired(store.G(ctx, tx)).Scan(&accounts).Error; err != nil {
			return err
		}
		for _, a := range accounts {
			if err := audit.Audit(ctx, tx, 0, "account.automatic_recovery", "account", itoa(a.ID), map[string]any{"plugin_key": a.PluginKey, "reason": "release time reached"}); err != nil {
				return err
			}
			if err := s.d.Events.Emit(ctx, tx, core.Event{Type: core.EventAccountStatusChanged,
				Payload: statusPayload(a.ID, a.PluginKey, a.Type, a.Name, "active", "automatic disable expired", nil)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, a := range accounts {
		s.changed(ctx, a.ID)
		s.kickCCGateway(a.ID, a.PluginKey, a.Type)
	}
	return len(accounts), nil
}

// recoverExpired makes up to 200 accounts whose timed automatic disable
// ended active again, skipping rows another sweep holds, and returns them.
func recoverExpired(db *gorm.DB) *gorm.DB {
	return db.Raw(`UPDATE accounts a SET status='active',status_reason='',auto_disabled_until=NULL,updated_at=clock_timestamp()
			FROM (SELECT id FROM accounts WHERE deleted_at IS NULL AND status='auto_disabled' AND auto_disabled_until<=clock_timestamp()
				ORDER BY auto_disabled_until,id LIMIT 200 FOR UPDATE SKIP LOCKED) due
			WHERE a.id=due.id RETURNING a.id,a.name,a.plugin_key,a.type`)
}

// recoveredAccount is an account SweepAutomaticState made active again.
type recoveredAccount struct {
	ID              int64
	Name, PluginKey string
	Type            string
}
