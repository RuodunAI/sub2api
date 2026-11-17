// Package hierarchy keeps a tree of rows (a forest) in one PostgreSQL table
// with a materialized path: every row lists its ancestors, from the root of
// its tree down to its parent, so the rows below one are a single indexed
// query (the path contains it) and the ancestors of one a single read,
// never a walk level by level. The core's users and their owners are one
// such tree; a plugin keeps its own on its own tables the same way.
//
// The table follows one convention (New names the columns so):
//
//	id        bigint PRIMARY KEY
//	parent_id bigint NULL REFERENCES <table>(id)   -- NULL: a root
//	path      bigint[] NOT NULL DEFAULT '{}'       -- the ancestors, root first
//	CREATE INDEX ... ON <table> USING gin (path)
//	CHECK (path[cardinality(path)] IS NOT DISTINCT FROM parent_id)
//
// A row is inserted as a root (parent and path left to their defaults) and
// placed with Move in the same transaction; Move is the only writer of the
// parent and the path, which change together. A row that is a parent is
// not deleted (the foreign key has no ON DELETE action): its id is in the
// paths below it.
package hierarchy

import (
	"errors"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// Tree is the tree of one table: its name and the names of its id, parent
// and path columns (constants of the code, never input), and how deep a
// row may lie.
type Tree struct {
	// Table is the table, schema-qualified or on the search_path.
	Table string
	// ID, Parent and Path are the columns of the convention (see the
	// package documentation) under the table's own names.
	ID, Parent, Path string
	// MaxDepth is the most ancestors a row may have; 0 sets no limit.
	MaxDepth int
}

// New is the Tree of table under the convention's column names.
func New(table string, maxDepth int) Tree {
	return Tree{Table: table, ID: "id", Parent: "parent_id", Path: "path", MaxDepth: maxDepth}
}

// Error is a Move the tree refuses. Its text after "hierarchy: " is a
// stable code (self, cycle, too_deep, not_found) that callers may hand on
// as their own.
type Error string

// The refusals of Move (and ErrNotFound of Ancestors).
const (
	ErrSelf     Error = "self"      // a row under itself
	ErrCycle    Error = "cycle"     // a row under one below it
	ErrTooDeep  Error = "too_deep"  // a row, or one below it, past MaxDepth
	ErrNotFound Error = "not_found" // the row or the parent is not in the table
)

func (e Error) Error() string { return "hierarchy: " + string(e) }

// Code is the code of e.
func (e Error) Code() string { return string(e) }

// errNoTx is Move's error on a handle outside a transaction, where its
// locks would end with their statements.
var errNoTx = errors.New("hierarchy: Move needs a transaction")

// Move places row id under parent (0: makes it a root) in tx, a READ
// COMMITTED transaction, and returns its new path: it writes the row's
// parent and path and rewrites the paths of every row below it, one UPDATE
// for them all. It refuses (Error) a parent that is the row itself, a row
// below it, or one that would put the row or a row below it more than
// MaxDepth levels deep, and a row or parent not in the table.
//
// Concurrent moves are safe: a move locks the root rows of the trees of
// the row and the parent (FOR NO KEY UPDATE, in id order) until tx ends, and
// reads both paths under those locks. Every path that changes belongs to
// the moved row's tree, so two moves that could make a cycle or a stale
// path together take a lock in common and the second sees the first; moves
// in other trees go on. A move whose tree another move puts under a
// third tree while it waits locks that tree's root too; when a move of
// that tree waits for it in turn, PostgreSQL ends one of the two with a
// deadlock error (40P01), which changes nothing: retry it. Callers that
// serialize their moves otherwise (the core holds its authz lock) never
// meet it.
func (t Tree) Move(tx *gorm.DB, id, parent int64) ([]int64, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if id == parent {
		return nil, ErrSelf
	}
	ids := []int64{id}
	if parent != 0 {
		ids = append(ids, parent)
	}
	paths, err := t.lockTrees(tx, ids)
	if err != nil {
		return nil, err
	}
	old, ok := paths[id]
	if !ok {
		return nil, ErrNotFound
	}
	path := []int64{}
	if parent != 0 {
		above, ok := paths[parent]
		switch {
		case !ok:
			return nil, ErrNotFound
		case slices.Contains(above, id):
			return nil, ErrCycle
		}
		path = append(slices.Clone(above), parent)
	}
	if t.MaxDepth > 0 {
		var deepest int
		if err := t.below(tx, id).Select("COALESCE(max(cardinality(?)), 0)", t.column(t.Path)).Scan(&deepest).Error; err != nil {
			return nil, err
		}
		if len(path)+max(deepest-len(old), 0) > t.MaxDepth {
			return nil, ErrTooDeep
		}
	}
	var parentID *int64
	if parent != 0 {
		parentID = &parent
	}
	err = t.rows(tx).Where("? = ?", t.column(t.ID), id).
		Updates(map[string]any{t.Parent: parentID, t.Path: pgdb.Array[int64](path)}).Error
	if err != nil {
		return nil, err
	}
	// Each path below holds id at len(old)+1 (1-based): what comes before
	// it is the old path, which the new one replaces.
	err = t.below(tx, id).Update(t.Path, gorm.Expr("?::bigint[] || ?[?:]", pgdb.Array[int64](path), t.column(t.Path), len(old)+1)).Error
	return path, err
}

// Ancestors is the path of row id: its ancestors, the root of its tree
// first and its parent last; empty for a root, ErrNotFound for no row.
func (t Tree) Ancestors(db *gorm.DB, id int64) ([]int64, error) {
	paths, err := t.paths(db, []int64{id})
	if err != nil {
		return nil, err
	}
	path, ok := paths[id]
	if !ok {
		return nil, ErrNotFound
	}
	return path, nil
}

// Below selects the ids of the rows below any of tops (their descendants,
// tops themselves not included), a subquery for another query built on
// db: "owner_id IN (?)". Its text is the same whatever tops holds (one
// array parameter, empty for none), and it reads the path's GIN index.
func (t Tree) Below(db *gorm.DB, tops ...int64) *gorm.DB {
	return t.rows(db.Session(&gorm.Session{NewDB: true})).Select("?", t.column(t.ID)).
		Where("? && ?", t.column(t.Path), pgdb.Array[int64](append([]int64{}, tops...)))
}

// lockTrees locks the roots of the trees of ids (see Move) and returns the
// paths of those of ids in the table, read once the roots they name are
// all locked: a move that commits while a lock is awaited may have put a
// row in another tree, whose root is then locked as well.
func (t Tree) lockTrees(tx *gorm.DB, ids []int64) (map[int64][]int64, error) {
	var held []int64
	for {
		paths, err := t.paths(tx, ids)
		if err != nil {
			return nil, err
		}
		var roots []int64
		for id, path := range paths {
			root := id
			if len(path) > 0 {
				root = path[0]
			}
			if !slices.Contains(held, root) && !slices.Contains(roots, root) {
				roots = append(roots, root)
			}
		}
		if len(roots) == 0 {
			return paths, nil
		}
		var locked []int64
		err = t.rows(tx).Select("?", t.column(t.ID)).Where("? = ANY(?)", t.column(t.ID), pgdb.Array[int64](roots)).
			Order(clause.OrderByColumn{Column: t.column(t.ID)}).Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).Scan(&locked).Error
		if err != nil {
			return nil, err
		}
		held = append(held, roots...)
	}
}

// paths reads the paths of those of ids in the table.
func (t Tree) paths(db *gorm.DB, ids []int64) (map[int64][]int64, error) {
	var rows []struct {
		ID   int64
		Path pgdb.Array[int64]
	}
	err := t.rows(db).Select("? AS id, ? AS path", t.column(t.ID), t.column(t.Path)).
		Where("? = ANY(?)", t.column(t.ID), pgdb.Array[int64](ids)).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Path
	}
	return out, nil
}

// below is the rows below id.
func (t Tree) below(db *gorm.DB, id int64) *gorm.DB {
	return t.rows(db).Where("? @> ?", t.column(t.Path), pgdb.Array[int64]{id})
}

// rows is a statement on the table.
func (t Tree) rows(db *gorm.DB) *gorm.DB { return db.Table("?", clause.Table{Name: t.Table}) }

func (t Tree) column(name string) clause.Column { return clause.Column{Name: name} }

// requireTx fails unless tx runs in a transaction.
func requireTx(tx *gorm.DB) error {
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errNoTx
	}
	return nil
}
