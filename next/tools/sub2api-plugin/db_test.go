package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

func TestIdempotentDDL(t *testing.T) {
	for in, want := range map[string]string{
		`CREATE TABLE "t" ("id" bigserial,PRIMARY KEY ("id"))`: `CREATE TABLE IF NOT EXISTS "t" ("id" bigserial,PRIMARY KEY ("id"))`,
		`ALTER TABLE "t" ADD "n" bigint NOT NULL DEFAULT 0`:    `ALTER TABLE "t" ADD COLUMN IF NOT EXISTS "n" bigint NOT NULL DEFAULT 0`,
		`ALTER TABLE "t" ADD CONSTRAINT "c" CHECK (n >= 0)`:    `DO $$ BEGIN ALTER TABLE "t" ADD CONSTRAINT "c" CHECK (n >= 0); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`CREATE INDEX IF NOT EXISTS "i" ON "t" ("n")`:          `CREATE INDEX IF NOT EXISTS "i" ON "t" ("n")`,
		`ALTER TABLE "t" ALTER COLUMN "n" SET NOT NULL`:        `ALTER TABLE "t" ALTER COLUMN "n" SET NOT NULL`,
	} {
		if got := idempotent(in); got != want {
			t.Errorf("idempotent(%s)\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestWriteMigration(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_init.sql", "0003_more.sql", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if path, err := writeMigration(dir, "plg_x", "models", &pgdb.DriftReport{}); path != "" || err != nil {
		t.Fatalf("no change wrote %q %v", path, err)
	}
	for _, r := range []pgdb.DriftReport{
		{Statements: []string{`ALTER TABLE "t" ADD "n" bigint`, `ALTER TABLE "t" ALTER COLUMN "s" TYPE varchar(9) USING "s"::varchar(9)`}},
		{Statements: []string{`ALTER TABLE "t" ADD "n" bigint`}, Unmapped: []string{"column t.old"}},
	} {
		if path, err := writeMigration(dir, "plg_x", "models", &r); !errors.Is(err, errManual) || path != "" {
			t.Fatalf("%+v wrote %q %v", r, path, err)
		}
	}
	path, err := writeMigration(dir, "plg_x", "add_n", &pgdb.DriftReport{Statements: []string{`ALTER TABLE "t" ADD "n" bigint`}})
	if err != nil || filepath.Base(path) != "0004_add_n.sql" {
		t.Fatalf("wrote %q %v", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(b), "-- plg_x 0004:") ||
		!strings.HasSuffix(string(b), "\n\nALTER TABLE \"t\" ADD COLUMN IF NOT EXISTS \"n\" bigint;\n") {
		t.Fatalf("migration:\n%s %v", b, err)
	}
}

// Every built-in plugin on GORM models has models that are its migrations,
// exactly. The image build packs without db check: this is where it runs.
func TestDBCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run on plugin modules and needs PostgreSQL")
	}
	t.Setenv("TEST_DATABASE_URL", testpg.URL(t))
	models, err := filepath.Glob(filepath.Join("..", "..", "plugins", "*", filepath.FromSlash(modelDir)))
	if err != nil || len(models) == 0 {
		t.Fatalf("no plugin with %s: %v", modelDir, err)
	}
	for _, m := range models {
		dir := filepath.Dir(filepath.Dir(m))
		if out := runOK(t, "db", "check", "--dir", dir); !strings.Contains(out, "match") {
			t.Errorf("db check %s: %s", dir, out)
		}
	}
	if out := runFail(t, "db", "check", "--dir", filepath.Join("..", "..", "plugins", "mailer")); !strings.Contains(out, errNoModels.Error()) {
		t.Fatalf("db check without models: %s", out)
	}
}
