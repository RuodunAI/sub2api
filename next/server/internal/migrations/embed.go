// Package migrations embeds the core SQL migrations.
package migrations

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed *.sql
var FS embed.FS

// superseded holds earlier contents of core migrations that a development
// build applied and a later migration completes, as
// superseded/<that build's commit>/<migration id>, each build's differing
// files together. Only for differences a later migration repairs: 0099's
// first form (b5b2fb564) lacked the staged status in its COMMENT on
// permissions.status, which 0100 sets again. A file here never changes.
//
//go:embed superseded
var superseded embed.FS

// SupersededBuilds returns, per development build in superseded, the
// contents of the migrations it applied that differ from FS, by id. The core
// accepts a database that applied one of them (store.CoreTracker), and a
// migration permit whose source schema is that build's contract
// (app.validateMigrationPermit).
func SupersededBuilds() (map[string]map[string][]byte, error) {
	files, err := fs.Glob(superseded, "superseded/*/*.sql")
	if err != nil {
		return nil, err
	}
	builds := map[string]map[string][]byte{}
	for _, f := range files {
		body, err := superseded.ReadFile(f)
		if err != nil {
			return nil, err
		}
		dir, id := path.Split(f)
		build := path.Base(dir)
		if builds[build] == nil {
			builds[build] = map[string][]byte{}
		}
		builds[build][id] = body
	}
	return builds, nil
}

// FirstIdempotentMigration is the first core migration from which every
// migration must be safe to run again (CREATE ... IF NOT EXISTS, ADD COLUMN
// IF NOT EXISTS, one-off data fixes guarded so a re-run does not repeat
// them): a managed upgrade can be interrupted after a migration commits
// and before the node records it everywhere. Earlier migrations predate the
// rule and are immutable once deployed.
const FirstIdempotentMigration = "0026_account_auto_disable.sql"
