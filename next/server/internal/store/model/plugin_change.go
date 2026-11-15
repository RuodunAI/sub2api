package model

import "gorm.io/gorm"

// PluginChange returns set, the columns an UPDATE of a plugins row assigns,
// with the two every change of that row also writes: updated_at, and
// row_version moved on, the generation that reconcile passes and runtime
// registration compare.
func PluginChange(set map[string]any) map[string]any {
	set["updated_at"] = gorm.Expr("now()")
	set["row_version"] = gorm.Expr("row_version + 1")
	return set
}
