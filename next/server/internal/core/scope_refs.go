package core

import (
	"context"

	"gorm.io/gorm"
)

// Kinds of rows a scoped grant names (ScopeGrantClearer).
const (
	ScopeRefAccount = "account"
	ScopeRefGroup   = "group"
	ScopeRefProxy   = "proxy"
)

// ScopeGrantClearer clears the scoped grants naming deleted rows (authz
// implements it; CONTRACTS §21, §99.1).
type ScopeGrantClearer interface {
	// ClearScopeGrants deletes, inside tx, the scoped grants that name the
	// deleted rows ids of kind (ScopeRef*), audited by actorID (0 = the
	// system), and returns the new authz version, 0 when nothing went. The
	// clearer announces it itself once tx has committed.
	ClearScopeGrants(ctx context.Context, tx *gorm.DB, actorID int64, kind string, ids []int64) (int64, error)
}
