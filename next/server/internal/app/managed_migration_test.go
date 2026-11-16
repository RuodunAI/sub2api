package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/migrations"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestMigrationPermitOnlyAllowsApprovedAppendOnlyHistory(t *testing.T) {
	inventory, err := migrationInventory(fstest.MapFS{
		"0001.sql": {Data: []byte("CREATE TABLE old_data (id bigint);")},
		"0002.sql": {Data: []byte("ALTER TABLE old_data ADD COLUMN flag boolean;")},
		"0003.sql": {Data: []byte("CREATE INDEX ON old_data(id);")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		applied := map[string]string{}
		for _, m := range inventory[:n] {
			applied[m.name] = m.checksum
		}
		p := rc.PrepareRequest{AllowMigration: true, ExpectedSchemaBefore: inventoryContract(inventory[:1]), ExpectedSchemaAfter: inventoryContract(inventory)}
		if err := validateMigrationPermit(inventory, applied, p); err != nil {
			t.Fatalf("legal migration restart at prefix %d: %v", n, err)
		}
	}
	cases := []struct {
		name    string
		applied map[string]string
		permit  rc.PrepareRequest
	}{
		{"missing source", map[string]string{inventory[0].name: inventory[0].checksum}, rc.PrepareRequest{AllowMigration: true}},
		{"wrong source", map[string]string{inventory[0].name: inventory[0].checksum}, rc.PrepareRequest{ExpectedSchemaBefore: "wrong"}},
		{"wrong target", map[string]string{inventory[0].name: inventory[0].checksum}, rc.PrepareRequest{ExpectedSchemaBefore: inventoryContract(inventory[:1]), ExpectedSchemaAfter: "wrong"}},
		{"older than source", map[string]string{}, rc.PrepareRequest{ExpectedSchemaBefore: inventoryContract(inventory[:1])}},
		{"unknown future migration", map[string]string{"9999.sql": "future"}, rc.PrepareRequest{Bootstrap: true}},
		{"changed history", map[string]string{inventory[0].name: "changed"}, rc.PrepareRequest{Bootstrap: true}},
		{"gap", map[string]string{inventory[1].name: inventory[1].checksum}, rc.PrepareRequest{Bootstrap: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMigrationPermit(inventory, tc.applied, tc.permit); err == nil {
				t.Fatal("unsafe migration admitted")
			}
		})
	}
	if err := validateMigrationPermit(inventory, map[string]string{}, rc.PrepareRequest{Bootstrap: true, AllowMigration: true}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSchemaRejectsUnknownFutureMigration(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO schema_migrations(id,checksum) VALUES('9999_future.sql','unknown')`); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCoreSchema(ctx, db); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("older core accepted future schema: %v", err)
	}
	contract, err := SchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareCoreSchema(ctx, db, rc.PrepareRequest{AllowMigration: true, ExpectedSchemaBefore: contract}); err == nil {
		t.Fatal("migration permit bypassed future schema rejection")
	}
}

func TestMigrationAndPluginPreparationDoNotAdmitBusiness(t *testing.T) {
	for _, mode := range []string{"preparing", "prepared", "draining", "drained"} {
		m := &managedCore{mode: mode, prepare: rc.PrepareRequest{AllowMigration: true, CoordinatePlugins: true}}
		want := mode == "preparing" || mode == "prepared"
		if m.coordinateAllowed() != want {
			t.Fatalf("mode %s wrong plugin admission", mode)
		}
		if m.backgroundAllowed() || m.status(context.Background()).Ready {
			t.Fatalf("mode %s migration admitted business", mode)
		}
	}
	m := &managedCore{mode: "prepared", prepare: rc.PrepareRequest{AllowMigration: true}}
	if m.coordinateAllowed() {
		t.Fatal("schema permit silently granted plugin coordination")
	}
}

// productionSchema is the schema contract of core 0.1.288, the release in
// production: the migrations through 0098, as `sub2api schema-contract`
// prints it and the gateway's plan expects before the 0.1.290 upgrade
// (deploy/gateway/ovh/README.md, 1f73f300…9d4f). A change to any released
// migration changes it, and a database that applied the old bytes would
// refuse the new core.
const productionSchema = "1f73f300166fd6f94fc4332169fb1016c64ae88a2587d49da9294327b38b9d4f"

// The 0.1.290 upgrade as the primary-first plan runs it on production: a
// database at 0098 takes 0099-0101 under the one permit start-primary
// hands the core. 0100 refuses a database with two super admin holders
// after 0099 has committed; the core does not serve that schema and
// neither does 0.1.288 (fix forward or restore). Once the data is fixed,
// the same permit finishes the upgrade, and starting again with it
// changes nothing.
func TestManagedUpgradeFromProductionResumesWithTheSamePermit(t *testing.T) {
	const production = "0098_plugin_staged_grants.sql"
	ctx := t.Context()
	db := testutil.DBThrough(t, production)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	applied := func() map[string]string {
		t.Helper()
		m, err := (store.CoreTracker{}).Applied(ctx, db.Gorm)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	inventory, err := migrationInventory(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	released := slices.IndexFunc(inventory, func(m coreMigration) bool { return m.name == production }) + 1
	permit := rc.PrepareRequest{AllowMigration: true,
		ExpectedSchemaBefore: inventoryContract(inventory[:released]), ExpectedSchemaAfter: inventoryContract(inventory)}
	if permit.ExpectedSchemaBefore != productionSchema {
		t.Fatalf("the migrations through %s are not the ones 0.1.288 applied: contract %s", production, permit.ExpectedSchemaBefore)
	}

	// What 0.1.288 holds: a role with a grant, and two super admins, which
	// 0097's trigger refuses but a repair bypassing it may have left.
	exec(`INSERT INTO roles (key, name, builtin, superuser) VALUES ('super_admin', '{}', true, true), ('editor', '{}', false, false);
		INSERT INTO permissions (key, module, label, source) VALUES ('account:read', 'account', '{}', 'core');
		INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = 'editor';`)
	a, b := testutil.InsertUser(t, db, "a@x"), testutil.InsertUser(t, db, "b@x")
	exec(`INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE key = 'editor'`, b)
	testutil.ExecWithoutTrigger(t, db, "user_roles", "user_roles_one_superuser",
		`INSERT INTO user_roles (user_id, role_id) SELECT u, r.id FROM unnest($1::bigint[]) u, roles r WHERE r.key = 'super_admin'`, []int64{a, b})

	if _, err := prepareCoreSchema(ctx, db, permit); err == nil || !strings.Contains(err.Error(), "several super admins") {
		t.Fatalf("0100 must refuse two super admins: %v", err)
	}
	half := applied()
	if _, ok := half["0099_plugin_permission_meta.sql"]; !ok || len(half) != released+1 {
		t.Fatalf("0099 alone stays committed, got %d migrations", len(half))
	}
	if err := VerifyCoreSchema(ctx, db); err == nil {
		t.Fatal("the new core served a half-migrated schema")
	}
	if _, err := appliedPrefix(inventory[:released], half); err == nil {
		t.Fatal("core 0.1.288 accepted a schema with 0099")
	}

	// The fix 0.1.288's recover-superadmin makes on this schema: one holder stays.
	exec(`DELETE FROM user_roles WHERE user_id = $1 AND role_id = (SELECT id FROM roles WHERE key = 'super_admin')`, b)
	done, err := prepareCoreSchema(ctx, db, permit)
	if err != nil {
		t.Fatalf("the same permit must finish the upgrade: %v", err)
	}
	if want := []string{"0100_casbin_rule.sql", "0101_drop_jwt_sessions.sql"}; !slices.Equal(done[:min(len(done), 2)], want) {
		t.Fatalf("the rerun applied %v, want %v first", done, want)
	}
	var policy string
	if err := db.SQL.QueryRowContext(ctx, `SELECT string_agg(concat_ws(' ', ptype, v0, v1, NULLIF(v2, '')), ', ' ORDER BY ptype, v0, v1)
		FROM casbin_rule`).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("g u:%d r:super_admin, g u:%d r:editor, p r:editor account:read *, p r:super_admin * *", a, b); policy != want {
		t.Fatalf("policy %q, want %q", policy, want)
	}
	if done, err = prepareCoreSchema(ctx, db, permit); err != nil || len(done) != 0 {
		t.Fatalf("a third start with the permit: %v %v", done, err)
	}
}
