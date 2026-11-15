package billing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The ledger entry points take the user's balance lock: on a handle that
// is no transaction they refuse before any statement.
func TestLedgerNeedsTransaction(t *testing.T) {
	s, ctx, pool := &Service{}, context.Background(), store.DryRun()
	_, err := s.SettleTx(ctx, pool, 1, "r", nil)
	require.ErrorIs(t, err, store.ErrNoTx)
	_, err = s.ApplyTx(ctx, pool, core.LedgerChange{})
	require.ErrorIs(t, err, store.ErrNoTx)
}
