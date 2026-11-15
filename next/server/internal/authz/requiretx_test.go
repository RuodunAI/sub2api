package authz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Bump locks the authz_meta row until the transaction ends: without one it
// refuses before any statement.
func TestBumpNeedsTransaction(t *testing.T) {
	_, err := (&Service{}).Bump(context.Background(), store.DryRun())
	require.ErrorIs(t, err, store.ErrNoTx)
}
