package ccgateway

import (
	"context"
	"encoding/json"
	"io"
	"strconv"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ core.AccountLifecycleResource = (*Service)(nil)

func (s *Service) PrepareAccount(ctx context.Context, p core.AccountPreparation) error {
	if !s.runtimesOn(ctx) {
		return reasonError(core.ErrUnavailable, "not_configured")
	}
	if p.Type != "managed" || !isDraftKey(p.Key) {
		return core.ErrInvalidArgument
	}
	if err := s.checkProxy(ctx, p.ProxyID); err != nil {
		return err
	}
	g := store.G(ctx, s.DB.Pool)
	res := insertPreparation(g, p)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		matches, err := preparationMatches(g, p)
		if err != nil {
			return err
		}
		if !matches {
			return core.ErrConflict
		}
	}
	s.Kick(p.Key)
	return nil
}

// insertPreparation inserts the draft of preparation p unless its key
// exists. The settings are written as given: a nil value is NULL, not the
// column default.
func insertPreparation(db *gorm.DB, p core.AccountPreparation) *gorm.DB {
	return runtimes(db).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).
		Create(map[string]any{"key": p.Key, "proxy_id": p.ProxyID, "created_by": p.CreatedBy, "initial_settings": p.Settings})
}

// preparationMatches reports whether the open draft of p is the one p
// describes (same creator, proxy and settings).
func preparationMatches(db *gorm.DB, p core.AccountPreparation) (bool, error) {
	return rowExists(db, preparationDraft(db, p))
}

// preparationDraft is the open draft p describes.
func preparationDraft(db *gorm.DB, p core.AccountPreparation) *gorm.DB {
	return newAccountDraft(db, p.Key, &p.CreatedBy).
		Where("proxy_id IS NOT DISTINCT FROM ? AND initial_settings = ?::jsonb", p.ProxyID, p.Settings)
}

// AccountPreparationAction runs one action on a preparation's draft runtime
// like the draft endpoints do (serveRuntime): the account service checked
// the action, the method and the body.
func (s *Service) AccountPreparationAction(ctx context.Context, p core.AccountPreparation, method, action string, payload json.RawMessage) (any, error) {
	ok, err := s.touchDraftOf(ctx, p.Key, &p.CreatedBy, accountDraft)
	if err != nil || !ok {
		return nil, errOr(err, draftNotFound(core.ErrNotFound))
	}
	desired, err := s.draftDesired(ctx, p.Key, false)
	if err != nil {
		return nil, err
	}
	switch action {
	case "sync":
		if err := s.Reconcile(ctx, p.Key); err != nil {
			return nil, syncFailure(err)
		}
		return map[string]bool{"synced": true}, nil
	case "status":
		status, failure := s.runtimeStatus(ctx, desired)
		if failure != nil {
			return nil, failure
		}
		return draftStatus(desired, status), nil
	}
	path, failure := runtimeActionPath(action, desired, true)
	if failure != nil {
		return nil, failure
	}
	out, failure := s.runtimeAction(ctx, desired, true, action, method, path, payload)
	if failure != nil {
		return nil, failure
	}
	return out, nil
}

func (s *Service) UpdateAccountPreparation(ctx context.Context, tx pgx.Tx, p core.AccountPreparation) error {
	res := setDraftProxy(store.G(ctx, tx), p.Key, &p.CreatedBy, p.ProxyID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Service) CheckAccountPreparation(ctx context.Context, p core.AccountPreparation) (core.AccountPreparationFacts, error) {
	matches, err := preparationMatches(store.G(ctx, s.DB.Pool), p)
	if err != nil {
		return core.AccountPreparationFacts{}, err
	}
	if !matches {
		return core.AccountPreparationFacts{}, core.ErrConflict.WithMessage("Account preparation configuration changed")
	}
	return s.CheckDraft(ctx, p.Key, &p.CreatedBy)
}

func (s *Service) AdoptAccountPreparation(ctx context.Context, tx pgx.Tx, p core.AccountPreparation, id int64) error {
	return AdoptDraft(ctx, tx, p.Key, id, &p.CreatedBy)
}

func (s *Service) CancelAccountPreparation(ctx context.Context, p core.AccountPreparation) error {
	_, err := s.removeDraft(ctx, p.Key, true)
	return err
}

func (s *Service) AccountAction(ctx context.Context, id int64, action string) (any, error) {
	if action == "refresh_token" {
		return s.RefreshToken(ctx, id)
	}
	if action != "refresh_auth" {
		return nil, core.ErrUnsupported
	}
	key, created, err := s.ReauthDraft(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "created": created}, nil
}

func (s *Service) AccountSynchronization(ctx context.Context, id int64) (core.AccountSynchronization, error) {
	state := core.AccountSynchronization{Status: "pending", Code: "runtime_pending"}
	if !s.runtimesOn(ctx) {
		state.Code = "runtime_not_configured"
		return state, nil
	}
	desired, err := s.desired(ctx, id, false)
	if err != nil {
		return state, err
	}
	if desired.Deleted {
		return core.AccountSynchronization{Status: "applied"}, nil
	}
	response, close, err := s.runtimeRequest(ctx, desired.Key, "GET", "status", nil, desired.Revision)
	if err != nil {
		return state, transportError(err)
	}
	defer close()
	defer response.Body.Close()
	var observed struct {
		Status             string `json:"status"`
		Revision           string `json:"revision"`
		EnvironmentPending *bool  `json:"environment_pending"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&observed) != nil {
		return state, reasonError(core.ErrUnavailable, "runtime_unavailable")
	}
	if observed.EnvironmentPending != nil && *observed.EnvironmentPending {
		state.Code = "environment_requires_new_runtime"
		return state, nil
	}
	if observed.EnvironmentPending == nil && (desired.Locale != "" || desired.Timezone != "") {
		state.Code = "controller_outdated"
		return state, nil
	}
	if observed.Status == "ready" && observed.Revision == desired.Revision {
		return core.AccountSynchronization{Status: "applied"}, nil
	}
	return state, nil
}

func (s *Service) ReconcileAccount(ctx context.Context, id int64) error {
	if !s.runtimesOn(ctx) {
		return nil
	}
	return s.Reconcile(ctx, strconv.FormatInt(id, 10))
}
