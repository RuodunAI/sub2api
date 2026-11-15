package usage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Every settlement path starts with lockUsageRow's FOR UPDATE, which only
// holds inside a transaction.
func TestLockUsageRowNeedsTransaction(t *testing.T) {
	_, err := lockUsageRow(context.Background(), store.DryRun(), 1)
	require.ErrorIs(t, err, store.ErrNoTx)
}
