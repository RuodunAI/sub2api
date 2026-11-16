package casbinx

import (
	"context"
	"fmt"

	gormadapter "github.com/casbin/gorm-adapter/v3"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// table is the policy table, whose columns migration 0100 sizes to what
// they hold rather than to gorm-adapter's own struct.
const table = casbinrule.Table

// pgLoader reads the policy from PostgreSQL, each read in one read-only
// repeatable-read transaction of store.DB.TxOptions.
type pgLoader struct{ db *store.DB }

// NewLoader returns the Loader over db.
func NewLoader(db *store.DB) Loader { return &pgLoader{db: db} }

// Policy reads the authz and policy versions, the active actions and the
// p rows in one snapshot.
func (l *pgLoader) Policy(ctx context.Context) (Policy, error) {
	var p Policy
	err := l.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		q := store.G(ctx, tx)
		var err error
		if p.Version, p.PolicyVersion, err = store.AuthzVersions(q); err != nil {
			return err
		}
		var active []string
		if err := q.Raw(`SELECT key FROM permissions WHERE status = 'active'`).Scan(&active).Error; err != nil {
			return err
		}
		p.Active = make(map[string]bool, len(active))
		for _, k := range active {
			p.Active[k] = true
		}
		p.Rules, err = loadPolicyRows(q)
		return err
	})
	if err != nil {
		return Policy{}, fmt.Errorf("casbinx: read the policy: %w", err)
	}
	return p, nil
}

// loadPolicyRows reads the p rows through gorm-adapter, bound to tx and
// used for this one read only:
//   - TurnOffAutoMigrate first: otherwise the constructor AutoMigrates the
//     table to the adapter's struct, which would retype every column to
//     varchar(100) (and needs DDL rights);
//   - NewAdapterByDBUseTableName then only scopes a session of tx to the
//     table (no connection, transaction or finalizer of its own);
//   - LoadFilteredPolicy runs one SELECT on that session, inside tx, and
//     decodes the rows as the adapter's LoadPolicy would, into a scratch
//     model the rows are taken back from.
//
// The adapter never reaches an enforcer, so none of its write or
// transaction APIs (AddPolicy, SavePolicy, Transaction) can run; gormguard
// [casbin-api] rejects them in code.
func loadPolicyRows(tx *gorm.DB) ([]Rule, error) {
	h := tx.Session(&gorm.Session{}) // TurnOffAutoMigrate rewrites *h
	gormadapter.TurnOffAutoMigrate(h)
	a, err := gormadapter.NewAdapterByDBUseTableName(h, "", table)
	if err != nil {
		return nil, err
	}
	m, err := newModel()
	if err != nil {
		return nil, err
	}
	if err := a.LoadFilteredPolicy(m, gormadapter.Filter{Ptype: []string{"p"}}); err != nil {
		return nil, err
	}
	rows, err := m.GetPolicy("p", "p")
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, r := range rows {
		out = append(out, Rule{Ptype: "p", V0: r[0], V1: r[1], V2: r[2]})
	}
	return out, nil
}

// UserRoles reads uid's role subjects (its g rows), whether it is active
// (not deleted, status active) and the versions, in one statement (one
// snapshot, one round trip). An inactive user holds nothing, even while
// its g rows stay (a disabled user's, a soft-deleted user's).
func (l *pgLoader) UserRoles(ctx context.Context, uid int64) (UserRoles, error) {
	var ur UserRoles
	var roles store.Strings
	err := userRoles(store.G(ctx, l.db.Gorm), uid).Row().Scan(&ur.Version, &ur.PolicyVersion, &ur.Active, &roles)
	if err != nil {
		return UserRoles{}, fmt.Errorf("casbinx: read the roles of user %d: %w", uid, err)
	}
	if ur.Active {
		ur.Roles = roles
	}
	return ur, nil
}

// userRoles selects the versions, whether uid is active and its role
// subjects, sorted.
func userRoles(db *gorm.DB, uid int64) *gorm.DB {
	return db.Raw(`SELECT m.version, m.policy_version,
    EXISTS (SELECT 1 FROM users u WHERE u.id = @uid AND u.deleted_at IS NULL AND u.status = 'active'),
    ARRAY(`+casbinrule.RoleSubsOf("CAST(@uid AS bigint)")+` ORDER BY 1)
FROM authz_meta m WHERE m.id = 1`, map[string]any{"uid": uid})
}
