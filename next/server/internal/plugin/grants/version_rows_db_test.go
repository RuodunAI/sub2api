package grants

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// VersionGrantsQuery reads the set staged for a version when there is one,
// else the plugin's grants, as VersionRows does on pgx.
func TestVersionGrantsQueryPicksStagedSet(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	for _, sql := range []string{
		`INSERT INTO plugins (key, name, status) VALUES ('demo', '{"en":"demo"}', 'enabled')`,
		`INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash)
			VALUES ('demo', 'kv', '{}', 'granted', '1.0.0', 'x')`,
		`INSERT INTO plugin_versions (plugin_key, version, manifest, manifest_hash, package_sha256, package_size, signature_status)
			VALUES ('demo', '2.0.0', '{}', 'y', 'z', 1, 'unsigned')`,
		`INSERT INTO plugin_staged_grants (plugin_key, version, permission, scope, status, manifest_hash)
			VALUES ('demo', '2.0.0', 'lock', '{}', 'revoked', 'y')`,
	} {
		if _, err := db.SQL.ExecContext(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for version, want := range map[string]string{"1.0.0": "kv/granted/1.0.0", "2.0.0": "lock/revoked/2.0.0"} {
		var rows []struct{ Permission, Status, PluginVersion string }
		if err := VersionGrantsQuery(store.G(ctx, db.Gorm), "demo", version).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Permission+"/"+rows[0].Status+"/"+rows[0].PluginVersion != want {
			t.Errorf("%s: got %+v, want %s", version, rows, want)
		}
	}
}

// Holds checks the set an instance of the version runs with: the staged set
// of a version that has one, else the plugin's grants; a nil scope matches
// any scope, another one only an equal document.
func TestHoldsReadsTheVersionSet(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	for _, sql := range []string{
		`INSERT INTO plugins (key, name, status) VALUES ('demo', '{"en":"demo"}', 'enabled')`,
		`INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash)
			VALUES ('demo', 'kv', '{"prefix": "a"}', 'granted', '1.0.0', 'x'), ('demo', 'lock', '{}', 'revoked', '1.0.0', 'x')`,
		`INSERT INTO plugin_versions (plugin_key, version, manifest, manifest_hash, package_sha256, package_size, signature_status)
			VALUES ('demo', '2.0.0', '{}', 'y', 'z', 1, 'unsigned')`,
		`INSERT INTO plugin_staged_grants (plugin_key, version, permission, scope, status, manifest_hash)
			VALUES ('demo', '2.0.0', 'lock', '{}', 'granted', 'y')`,
	} {
		if _, err := db.SQL.ExecContext(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		version, permission string
		scope               []byte
		want                bool
	}{
		{"1.0.0", "kv", nil, true},
		{"1.0.0", "kv", []byte(`{"prefix":"a"}`), true},
		{"1.0.0", "kv", []byte(`{"prefix":"b"}`), false},
		{"1.0.0", "lock", nil, false}, // revoked
		{"2.0.0", "lock", []byte(`{}`), true},
		{"2.0.0", "kv", nil, false}, // the staged set is complete
		{"3.0.0", "kv", nil, true},  // no staged set: the plugin's grants
	} {
		got, err := Holds(ctx, db.Gorm, "demo", c.version, c.permission, c.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("Holds(%s, %s, %s) = %v, want %v", c.version, c.permission, c.scope, got, c.want)
		}
	}
}
