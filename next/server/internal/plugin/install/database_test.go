package install

import (
	"context"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/dbschema"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
)

// accessManifest is a manifest asking for access under db.core_read.
func accessManifest(version string, access ...manifest.DatabaseAccess) *manifest.Manifest {
	m := pkgtest.Guard("guard", version, "sub2api")
	m.Database.Access = access
	m.HostPermissions = append(m.HostPermissions, manifest.HostPermission{ID: dbschema.AccessPermission})
	return m
}

// Any widening of database.access - a privilege, a column, a table - is a
// wider db.core_read scope: the upgrade lists it as widened and its
// consent does not carry the grant over. Access kept or narrowed is
// carried.
func TestDatabaseAccessWideningIsConsentedAgain(t *testing.T) {
	selectUsers := manifest.DatabaseAccess{Table: "users", Columns: []string{"id", "email"}, Privileges: []string{"select"}}
	old := accessManifest("0.1.0", selectUsers)
	hp := old.HostPermissions[len(old.HostPermissions)-1]
	current := map[string]Grant{dbschema.AccessPermission: {Permission: dbschema.AccessPermission, Risk: riskOf(hp.ID),
		Scope: requestedScope(old, hp), Status: GrantGranted, PluginVersion: "0.1.0"}}
	if riskOf(hp.ID) != manifest.RiskCritical {
		t.Fatalf("risk of %s = %s", hp.ID, riskOf(hp.ID))
	}

	widened := func(m *manifest.Manifest) bool {
		for _, it := range diffGrants(m, current).Widened {
			if it.ID == dbschema.AccessPermission {
				return true
			}
		}
		return false
	}
	carried := func(m *manifest.Manifest) bool {
		out, errs := decide(m, ConsentRequest{}, current, nil, true)
		for _, d := range out {
			if d.permission == dbschema.AccessPermission {
				return d.status == GrantGranted && d.carried
			}
		}
		for _, e := range errs {
			if e.Field == "permission:"+dbschema.AccessPermission && e.Code != "consent_required" {
				t.Fatalf("decide: %+v", e)
			}
		}
		return false
	}

	for name, access := range map[string][]manifest.DatabaseAccess{
		"select to select+update": {{Table: "users", Columns: []string{"id", "email"}, Privileges: []string{"select", "update"}}},
		"another column":          {{Table: "users", Columns: []string{"id", "email", "status"}, Privileges: []string{"select"}}},
		"every column":            {{Table: "users", Privileges: []string{"select"}}},
		"another table":           {selectUsers, {Table: "groups", Columns: []string{"id"}, Privileges: []string{"select"}}},
	} {
		m := accessManifest("0.2.0", access...)
		if !widened(m) {
			t.Errorf("%s: db.core_read not widened: %+v", name, diffGrants(m, current))
		}
		if carried(m) {
			t.Errorf("%s: db.core_read carried over", name)
		}
	}
	for name, access := range map[string][]manifest.DatabaseAccess{
		"same":     {selectUsers},
		"narrower": {{Table: "users", Columns: []string{"id"}, Privileges: []string{"select"}}},
	} {
		m := accessManifest("0.2.0", access...)
		if widened(m) {
			t.Errorf("%s: db.core_read widened", name)
		}
		if !carried(m) {
			t.Errorf("%s: db.core_read not carried over", name)
		}
	}

	// The scope the grant stores is the access, so the identity's grants
	// (dbschema.SyncAccessTx) follow what the administrator approved.
	if got := pkg.NormalizeScope(dbschema.AccessScope([]manifest.DatabaseAccess{
		{Table: "users", Columns: []string{"id"}, Privileges: []string{"select", "update", "delete"}},
	}))["access"]; len(got.([]any)) != 3 || got.([]any)[2] != "delete:users.*" {
		t.Fatalf("access scope = %v", got)
	}
}

// The review of a version lists its database access and its migration
// scripts with what installing it does with each, and GET
// /plugins/:key/database shows the scripts with when each was applied.
func TestReviewAndDatabaseShowMigrations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := accessManifest("0.1.0", manifest.DatabaseAccess{Table: "users", Columns: []string{"id"}, Privileges: []string{"select"}})
	r, err := e.svc.Upload(ctx, pkgtest.Build(m, e.root), e.admin, UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DatabaseAccess) != 1 || r.DatabaseAccess[0].Table != "users" {
		t.Fatalf("database_access = %+v", r.DatabaseAccess)
	}
	if len(r.DatabaseMigrations) != 1 || r.DatabaseMigrations[0].ID != "0001_init.sql" || r.DatabaseMigrations[0].Status != MigrationNew ||
		r.DatabaseMigrations[0].SQL == "" || r.DatabaseMigrations[0].Checksum == "" {
		t.Fatalf("database_migrations = %+v", r.DatabaseMigrations)
	}
	sum := r.DatabaseMigrations[0].Checksum

	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for checksum, status := range map[string]string{sum: MigrationApplied, "other": MigrationChanged} {
		e.schemas.applied = map[string]core.PluginMigrationRecord{"0001_init.sql": {Checksum: checksum, AppliedAt: at}}
		r, err := e.svc.Review(ctx, "guard", "0.1.0", e.admin)
		if err != nil {
			t.Fatal(err)
		}
		if r.DatabaseMigrations[0].Status != status {
			t.Fatalf("applied with %s: status %s, want %s", checksum, r.DatabaseMigrations[0].Status, status)
		}
	}

	d, err := e.svc.Database(ctx, "guard")
	if err != nil {
		t.Fatal(err)
	}
	if d.Prefix != "plg_guard__" || d.Role != "plg1_guard" || len(d.Access) != 1 || len(d.Migrations) != 1 ||
		d.Migrations[0].AppliedAt == nil || !d.Migrations[0].AppliedAt.Equal(at) || d.Migrations[0].Checksum != sum {
		t.Fatalf("database = %+v", d)
	}
	e.schemas.applied = nil
	if d, err = e.svc.Database(ctx, "guard"); err != nil || d.Migrations[0].AppliedAt != nil {
		t.Fatalf("database before the migration = %+v (%v)", d, err)
	}
	if _, err := e.svc.Database(ctx, "nope"); err == nil {
		t.Fatal("database of an unknown plugin")
	}
}

// An upload asking for a table or column the core does not have is
// rejected.
func TestUploadRejectsUnknownAccess(t *testing.T) {
	e := newEnv(t)
	e.schemas.missing = map[string]bool{"nope": true}
	m := accessManifest("0.1.0", manifest.DatabaseAccess{Table: "nope", Privileges: []string{"select"}})
	_, err := e.svc.Upload(context.Background(), pkgtest.Build(m, e.root), e.admin, UploadOptions{})
	if fieldCode(err, "database.access[0].table") != "unknown" {
		t.Fatalf("upload: %v", err)
	}
}
