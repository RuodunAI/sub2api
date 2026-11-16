package core

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Kinds of rows a scoped grant names (ClearScopeGrants).
const (
	ScopeRefAccount = "account"
	ScopeRefGroup   = "group"
	ScopeRefProxy   = "proxy"
)

// scopeRefPatterns match the scopes naming a row of kind (CONTRACTS §21.0):
// account/X and group/G/account/X; group/G and group/G/account/X; proxy/X.
var scopeRefPatterns = map[string]string{
	ScopeRefAccount: `(^|/)account/%d$`,
	ScopeRefGroup:   `^group/%d(/|$)`,
	ScopeRefProxy:   `^proxy/%d$`,
}

// ClearScopeGrants deletes, inside tx, the scoped grants that name the
// deleted rows ids of kind, audited as authz.scope.clear by actorID (0 =
// the system) per role, and moves the authz version when any went. A grant
// naming a deleted row reaches nothing anyway; clearing it keeps roles
// readable and a reused id from being reached. It returns the new authz
// version, 0 when nothing went: the caller announces it after commit
// (AnnounceAuthzChanged), as every authz write does.
func ClearScopeGrants(ctx context.Context, tx *gorm.DB, actorID int64, kind string, ids []int64) (int64, error) {
	pattern, ok := scopeRefPatterns[kind]
	if !ok {
		return 0, fmt.Errorf("core: unknown scope reference kind %q", kind)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	patterns := make([]string, len(ids))
	for i, id := range ids {
		patterns[i] = fmt.Sprintf(pattern, id)
	}
	var actor *int64
	if actorID > 0 {
		actor = &actorID
	}
	// The authz lock first, as every authz write takes it (a role save in
	// flight would otherwise deadlock with this delete), and only when
	// there is something to clear, so that deletes do not queue behind it.
	db := store.G(ctx, tx)
	found, err := store.Exists(db, scopeGrants(db, patterns))
	if err != nil || !found {
		return 0, err
	}
	if err := db.Exec(`SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE`).Error; err != nil {
		return 0, err
	}
	// No row: cleared meanwhile, before the lock; the version stays 0.
	var version int64
	err = clearScopeGrants(db, patterns, actor, kind, ids).Scan(&version).Error
	return version, err
}

// scopeGrants selects the scoped grants matching patterns.
func scopeGrants(db *gorm.DB, patterns []string) *gorm.DB {
	return casbinrule.ScopedGrants(db, patterns)
}

// clearScopeGrants deletes the scoped grants matching patterns, audits them
// per role and moves the authz version, returning it when any went.
func clearScopeGrants(db *gorm.DB, patterns []string, actor *int64, kind string, ids []int64) *gorm.DB {
	return db.Raw(`WITH cleared AS (
    `+casbinrule.DeleteScopedSQL+`
), audited AS (
    INSERT INTO audit_logs (user_id, action, target_type, target_id, detail)
    SELECT ?, 'authz.scope.clear', 'role', role_id::text,
        jsonb_build_object('kind', ?::text, 'ids', to_jsonb(?::bigint[]), 'keys', to_jsonb(array_agg(key ORDER BY key)))
    FROM cleared GROUP BY role_id
)
UPDATE authz_meta SET version = version + 1 WHERE id = 1 AND EXISTS (SELECT 1 FROM cleared)
RETURNING version`, store.Strings(patterns), actor, kind, store.Int64s(ids))
}
