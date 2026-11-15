package authz

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// CasbinDriftTask is the cluster task that checks the casbin_rule
// projection against the legacy role tables and repairs it (CONTRACTS §99).
const CasbinDriftTask = "core.authz.casbin_drift"

const casbinDriftEvery = 10 * time.Minute

// casbinDriftSpec registers CasbinDriftTask: the triggers keep casbin_rule
// in step with every writer in the writer's transaction, so drift means a
// writer bypassed them (TRUNCATE, session_replication_role = replica, hand
// written SQL). The role tables are the truth, so repairing is safe.
func (s *Service) casbinDriftSpec() core.TaskSpec {
	return core.TaskSpec{
		Name:  CasbinDriftTask,
		Title: core.LocalizedText{"en": "Check the Casbin policy projection", "zh": "校验 Casbin 策略投影"},
		Description: core.LocalizedText{
			"en": "Compares casbin_rule with the roles and grants it projects and, on a difference, repairs it from them and advances the permission version.",
			"zh": "比对 casbin_rule 与它所投影的角色和授予；有差异时按角色表修正，并升权限版本。",
		},
		Scope:   core.TaskCluster,
		Every:   casbinDriftEvery,
		Timeout: time.Minute,
		Handler: func(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
			n, fixed, err := s.RepairCasbinDrift(ctx)
			if err != nil || n == 0 {
				return core.TaskResult{}, err
			}
			return core.TaskResult{Count: int(fixed), Message: fmt.Sprintf("%d rows drifted, %d repaired", n, fixed)}, nil
		},
	}
}

// RepairCasbinDrift counts the rows of authz_casbin_drift and, when there
// are any, repairs casbin_rule (authz_casbin_repair) in an authz mutation:
// the version moves on, so every node rebuilds its Casbin snapshot and drops
// the sets it resolved from the drifted rows. It returns the drift and the
// rows repaired.
func (s *Service) RepairCasbinDrift(ctx context.Context) (drift, fixed int64, err error) {
	if err := casbinDrift(store.G(ctx, s.db.Gorm)).Row().Scan(&drift); err != nil {
		return 0, 0, err
	}
	if drift == 0 {
		return 0, 0, nil
	}
	slog.WarnContext(ctx, "authz: casbin_rule drifted from the role tables; repairing", "rows", drift)
	err = s.mutate(ctx, func(tx *gorm.DB) error {
		var n sql.NullInt64
		if err := casbinRepair(store.G(ctx, tx)).Row().Scan(&n); err != nil {
			return err
		}
		fixed = n.Int64
		return nil
	})
	return drift, fixed, err
}

// casbinDrift counts the rows casbin_rule has too many or too few.
func casbinDrift(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT count(*) FROM authz_casbin_drift`)
}

// casbinRepair rewrites casbin_rule to authz_casbin_expected and returns
// the rows it changed.
func casbinRepair(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT authz_casbin_repair()`)
}
