// Package permvectors holds the shared test vectors of scoped permission
// keys (CONTRACTS §21): canonical spellings, the implies table and the row
// check (core.DataScope.AllowsRow, the console's scopeAllowsRow). The
// server's perm package and the console's TypeScript implementation run the
// same file, so the two cannot drift.
package permvectors

import _ "embed"

// JSON is vectors.json:
//
//	{"canonical": [{"in": "...", "out": "..."} | {"in": "...", "invalid": true}],
//	 "implies":   [{"have": "...", "want": "...", "ok": bool}],
//	 "allows":    [{"have": ["..."], "action": "...", "me": 7,
//	               "row": {"id": 1, "created_by": 7 | null, "group_ids": [12] | null}, "ok": bool}]}
//
// A row's created_by is its owner column (core.ScopeTable: an account's
// owner_id, a proxy's created_by). The allows vectors judge rows in
// memory, where a team reaches what own does: the rows of the users below
// its holder are the server's statements' to read (core.UserTree).
//
//go:embed vectors.json
var JSON []byte
