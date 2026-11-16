package rollout

import (
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/rows"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Statement pieces shared by the rollout state machine (GORM, see package
// store).

var (
	sqlNow = gorm.Expr("now()")
	// bumpRowVersion moves row_version, the fence of plugins and
	// plugin_rollouts: every compare-and-set reads it first.
	bumpRowVersion = gorm.Expr("row_version + 1")
	// leaseFromNow is the coordinator lease end, ms milliseconds after the
	// statement's clock_timestamp().
	leaseFromNow = "clock_timestamp() + ? * interval '1 millisecond'"
)

// uninstallPending selects the uninstall marker of a plugin.
func uninstallPending(g *gorm.DB, key string) *gorm.DB {
	return g.Model(&model.PluginUninstall{}).Select("1").Where("plugin_key = ?", key)
}

// openRollouts filters the rollouts of a plugin that have not finished.
func openRollouts(g *gorm.DB, key string) *gorm.DB {
	return g.Model(&model.PluginRollout{}).Where("plugin_key = ? AND phase IN ('preparing','activating')", key)
}

// updatePlugin writes set to the plugin row, as a change of it
// (rows.Change).
func updatePlugin(g *gorm.DB, key string, set map[string]any) *gorm.DB {
	return g.Model(&model.Plugin{}).Where("key = ?", key).Updates(rows.Change(set))
}

// rolloutCols are the plugin_rollouts columns of a rolloutRow, read into a
// rolloutRecord.
const rolloutCols = `id, plugin_key, action, from_version, target_version, phase,
	COALESCE(coordinator_node_id, '') AS coord_node, COALESCE(coordinator_boot_id, '') AS coord_boot,
	(coordinator_lease_until IS NULL OR coordinator_lease_until < clock_timestamp()) AS lease_expired,
	error, row_version,
	EXTRACT(EPOCH FROM now() - created_at)::float8 AS age_sec, EXTRACT(EPOCH FROM now() - updated_at)::float8 AS phase_age_sec`

// rolloutRecord is the scan target of rolloutCols; rolloutRow keeps its
// fields unexported.
type rolloutRecord struct {
	ID            int64   `gorm:"column:id"`
	PluginKey     string  `gorm:"column:plugin_key"`
	Action        string  `gorm:"column:action"`
	FromVersion   *string `gorm:"column:from_version"`
	TargetVersion *string `gorm:"column:target_version"`
	Phase         string  `gorm:"column:phase"`
	CoordNode     string  `gorm:"column:coord_node"`
	CoordBoot     string  `gorm:"column:coord_boot"`
	LeaseExpired  bool    `gorm:"column:lease_expired"`
	Error         string  `gorm:"column:error"`
	RowVersion    int64   `gorm:"column:row_version"`
	AgeSec        float64 `gorm:"column:age_sec"`
	PhaseAgeSec   float64 `gorm:"column:phase_age_sec"`
}

func (r *rolloutRecord) row() *rolloutRow {
	return &rolloutRow{
		id: r.ID, key: r.PluginKey, action: r.Action, from: r.FromVersion, target: r.TargetVersion,
		phase: r.Phase, coordNode: r.CoordNode, coordBoot: r.CoordBoot, leaseExpire: r.LeaseExpired,
		err: r.Error, rowVersion: r.RowVersion, ageSec: r.AgeSec, phaseAgeSec: r.PhaseAgeSec,
	}
}

// selectRollouts selects rolloutCols from plugin_rollouts.
func selectRollouts(g *gorm.DB) *gorm.DB {
	return g.Model(&model.PluginRollout{}).Select(rolloutCols)
}
