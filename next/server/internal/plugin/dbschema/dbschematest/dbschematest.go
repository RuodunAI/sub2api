// Package dbschematest builds a dbschema.Manager for the tests of other
// packages.
package dbschematest

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// masterKey is the master key of every manager New builds: the managers of
// the nodes of a test share it, as the nodes of a deployment do, so that
// they derive the same password for a plugin's identity.
var masterKey = sha256.Sum256([]byte("dbschematest"))

// New builds the manager on db, whose DSN is dsn. The test drops what it
// creates (Manager.Drop with purge).
func New(t testing.TB, db *store.DB, dsn string) *dbschema.Manager {
	t.Helper()
	m, err := dbschema.New(context.Background(), db, dsn, masterKey[:], nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
