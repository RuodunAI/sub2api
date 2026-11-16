package install

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// purgeTx runs fn in a transaction; with purge, fn calls
// Deps.Accounts.PurgePluginAccounts, which commits a transaction of its own
// while fn's keeps the plugin's rows locked (Uninstall, ReleaseRetiredKey).
//
// The second transaction is on purpose: the purge's work after its commit
// (cooldowns, the authz and account broadcasts) must follow that commit,
// and a purge that committed stays done for the retry of an uninstall that
// fails after it. Its statements touch the plugin's accounts and what hangs
// off them, never the rows fn locked first, so it does not wait for fn's
// transaction in the database.
//
// It does wait for a second pool connection while fn holds one, and were
// every connection held by such a wait, none would come back. purgeMu
// admits one such transaction per node at a time, and every other holder
// of a connection gives it back without taking a second (gormguard
// [tx-pool] keeps direct uses of the pool out of transaction callbacks), so
// with a pool of two connections or more the second one always comes. A
// pool of one could never serve it: purging is refused there.
func (s *Service) purgeTx(ctx context.Context, purge bool, fn func(tx *gorm.DB) error) error {
	if !purge {
		return s.d.DB.Tx(ctx, fn)
	}
	if s.d.DB.MaxConns() == 1 {
		return core.ErrUnavailable.WithMessage("purging a plugin's accounts needs a database pool of at least two connections")
	}
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	return s.d.DB.Tx(ctx, fn)
}
