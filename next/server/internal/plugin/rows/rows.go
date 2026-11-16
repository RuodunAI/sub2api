// Package rows holds what every write of a plugins row shares, for the
// plugin packages that write one (install, rollout, api).
package rows

import (
	"maps"

	"gorm.io/gorm"
)

// Change returns the columns an UPDATE of a plugins row assigns: set, which
// it does not modify, and the two that every change of that row also
// writes: updated_at, and row_version moved on, the generation that
// reconcile passes and runtime registration compare.
func Change(set map[string]any) map[string]any {
	out := maps.Clone(set)
	if out == nil {
		out = map[string]any{}
	}
	out["updated_at"] = gorm.Expr("now()")
	out["row_version"] = gorm.Expr("row_version + 1")
	return out
}
