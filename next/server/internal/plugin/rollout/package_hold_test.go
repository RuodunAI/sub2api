package rollout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema/dbschematest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// sweepMidMigration runs a package sweep after the migration read its first
// file and before the schema manager reads the rest, as a drain finishing on
// another goroutine or the end of a reconcile pass would.
type sweepMidMigration struct {
	Schemas
	sweep func()
}

func (s sweepMidMigration) Migrate(ctx context.Context, key string, fsys fs.FS) ([]string, error) {
	if _, err := fs.ReadFile(fsys, "0001.sql"); err != nil {
		return nil, err
	}
	s.sweep()
	return s.Schemas.Migrate(ctx, key, fsys)
}

// The coordinator of a rollout opens the target package outside a reconcile
// pass. Until a pass reads the new rollout, the target is not referenced, so
// a sweep used to release (close) the package under the running migration:
// "migration failed: read .../package.s2plugin: file already closed".
func TestSweepKeepsPackageWhileCoordinatorMigrates(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	key := "hold" + hex.EncodeToString(b)
	m := registrytest.Manifest(key, "2.0.0")
	data := registrytest.Package(t, m, []byte("bin"), map[string][]byte{
		"migrations/0001.sql": []byte(`CREATE TABLE IF NOT EXISTS plg_` + key + `__a (id int);`),
		"migrations/0002.sql": []byte(`CREATE TABLE IF NOT EXISTS plg_` + key + `__b (id int);`),
	})
	registrytest.Install(t, db, m, data, map[string]string{"db.schema": "{}"}, "enabled")
	schemas := dbschematest.New(t, db, testutil.DSN(db))
	t.Cleanup(func() { _ = schemas.Drop(context.Background(), key, true) })

	c := newTestController(t, &stubRuntime{}, nil, t.TempDir())
	c.o.DB = db
	pkgs := registry.NewPackages(db, t.TempDir(), registry.WithSource(registrytest.Source()))
	t.Cleanup(pkgs.Close)
	c.o.Packages = pkgs
	c.o.Schemas = sweepMidMigration{Schemas: schemas, sweep: c.sweepPackages}

	target := "2.0.0"
	if err := c.migrate(ctx, &rolloutRow{key: key, target: &target}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied, err := schemas.AppliedMigrations(ctx, key); err != nil || len(applied) != 2 {
		t.Fatalf("applied migrations = %v (%v), want 2", applied, err)
	}
	// Once the migration is done the package is no longer held.
	c.sweepPackages()
	if refs := pkgs.Cached(); len(refs) != 0 {
		t.Fatalf("unreferenced package still cached after the migration: %v", refs)
	}
}
