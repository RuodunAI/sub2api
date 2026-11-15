package model_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/modelgen"
)

// The checked-in models must be exactly what the generator makes from the
// current migrations, so a migration never lands without its models.
func TestModelsMatchMigrations(t *testing.T) {
	files, err := modelgen.FromMigrations(t.Context(), testpg.URL(t), modelgen.Default)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, modelgen.Write(dir, files))

	want, err := modelgen.ReadGenerated(dir)
	require.NoError(t, err)
	have, err := modelgen.ReadGenerated(".")
	require.NoError(t, err)

	var stale []string
	for name, src := range want {
		if string(have[name]) != string(src) {
			stale = append(stale, name)
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("internal/store/model is out of date with the migrations (%v);\nregenerate from next/server with: go run ./cmd/gen-models", stale)
	}
}
