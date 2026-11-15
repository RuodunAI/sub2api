// Command gen-models regenerates internal/store/model, the GORM models of
// the core tables, from a scratch database migrated with the core
// migrations. Run it from next/server after adding or changing a migration:
//
//	go run ./cmd/gen-models [-dsn postgres://...] [-out internal/store/model]
//
// The scratch database is created on the server of -dsn (default
// TEST_DATABASE_URL, else the local test PostgreSQL of sdk/testpg) and
// dropped afterwards. Tables and type mappings are set by modelgen.Default.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/modelgen"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("TEST_DATABASE_URL"), "superuser DSN of the server for the scratch database")
	out := flag.String("out", "internal/store/model", "output directory")
	flag.Parse()

	ctx := context.Background()
	if *dsn == "" {
		var err error
		if *dsn, err = testpg.Ensure(ctx); err != nil {
			log.Fatalf("local test PostgreSQL: %v (or pass -dsn)", err)
		}
	}
	files, err := modelgen.FromMigrations(ctx, *dsn, modelgen.Default)
	if err != nil {
		log.Fatal(err)
	}
	if err := modelgen.Write(*out, files); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d files to %s", len(files), *out)
}
