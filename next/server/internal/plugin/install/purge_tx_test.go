package install

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// A transaction that purges accounts in a second one needs two pool
// connections: a pool of one refuses it before taking its first, and still
// runs every other transaction.
func TestPurgeTxNeedsTwoConnections(t *testing.T) {
	ctx := context.Background()
	one, err := store.OpenSized(ctx, testutil.DSN(testutil.DB(t)), 1)
	require.NoError(t, err)
	t.Cleanup(one.Close)
	s := &Service{d: Deps{DB: one}}
	ran := 0
	fn := func(*gorm.DB) error { ran++; return nil }

	err = s.purgeTx(ctx, true, fn)
	require.Equal(t, core.ErrUnavailable.Code, core.AsError(err).Code, "%v", err)
	require.Zero(t, ran)
	require.NoError(t, s.purgeTx(ctx, false, fn))
	require.Equal(t, 1, ran)
}
