package authz

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// VerifyTargets lists what sub2api admin authz-verify compares: every user
// id, and the keys to ask Enforce: each catalog key (any status), the self
// keys and each scoped key some role grants.
func (s *Service) VerifyTargets(ctx context.Context) (users []int64, keys []string, err error) {
	db := store.G(ctx, s.db.Gorm)
	if err = verifyUsers(db).Scan(&users).Error; err != nil {
		return nil, nil, err
	}
	if err = verifyKeys(db).Scan(&keys).Error; err != nil {
		return nil, nil, err
	}
	return users, append(keys, SelfPermissionKeys()...), nil
}

// verifyUsers selects every user id, deleted and disabled ones included
// (both engines must grant them nothing).
func verifyUsers(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT id FROM users ORDER BY id`)
}

// verifyKeys selects each catalog key and each granted scoped key.
func verifyKeys(db *gorm.DB) *gorm.DB {
	return db.Raw(`
SELECT key FROM permissions
UNION
SELECT p.key || ':' || rs.scope FROM role_permission_scopes rs JOIN permissions p ON p.id = rs.permission_id
ORDER BY 1`)
}
