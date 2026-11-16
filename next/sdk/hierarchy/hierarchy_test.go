package hierarchy_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/hierarchy"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

// newTree makes a table of the convention in a schema of its own, dropped
// after the test, and returns its Tree (depth at most maxDepth) and a
// handle with room for concurrent transactions.
func newTree(t *testing.T, maxDepth int) (hierarchy.Tree, *gorm.DB) {
	t.Helper()
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgdb.Close(db) })
	schema := fmt.Sprintf("hier_%d_%d", time.Now().UnixNano(), rand.IntN(1<<20))
	require.NoError(t, db.Exec(`CREATE SCHEMA `+schema+`;
		CREATE TABLE `+schema+`.node (
			id bigserial PRIMARY KEY,
			parent_id bigint REFERENCES `+schema+`.node(id),
			path bigint[] NOT NULL DEFAULT '{}',
			CHECK (path[cardinality(path)] IS NOT DISTINCT FROM parent_id));
		CREATE INDEX ON `+schema+`.node USING gin (path)`).Error)
	t.Cleanup(func() { _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`).Error })
	return hierarchy.New(schema+".node", maxDepth), db
}

// add inserts n roots and returns their ids.
func add(t *testing.T, tree hierarchy.Tree, db *gorm.DB, n int) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		require.NoError(t, db.Raw(`INSERT INTO `+tree.Table+` DEFAULT VALUES RETURNING id`).Scan(&ids[i]).Error)
	}
	return ids
}

// move is Move in a transaction of its own.
func move(tree hierarchy.Tree, db *gorm.DB, id, parent int64) ([]int64, error) {
	var path []int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		path, err = tree.Move(tx, id, parent)
		return err
	})
	return path, err
}

// below lists the rows below tops, sorted.
func below(t *testing.T, tree hierarchy.Tree, db *gorm.DB, tops ...int64) []int64 {
	t.Helper()
	var ids []int64
	require.NoError(t, db.Table(tree.Table).Where("id IN (?)", tree.Below(db, tops...)).Order("id").Pluck("id", &ids).Error)
	return ids
}

// consistent checks that every path is its parent's path and the parent,
// and that no row is below itself.
func consistent(t *testing.T, tree hierarchy.Tree, db *gorm.DB) {
	t.Helper()
	var bad []int64
	require.NoError(t, db.Raw(`SELECT n.id FROM `+tree.Table+` n LEFT JOIN `+tree.Table+` p ON p.id = n.parent_id
		WHERE n.path <> CASE WHEN p.id IS NULL THEN '{}' ELSE p.path || p.id END OR n.id = ANY(n.path)`).Scan(&bad).Error)
	require.Empty(t, bad, "rows whose path is not their parent's")
}

// A move writes the parent and the path of the row and rewrites the paths
// below it; Ancestors reads a path, Below the rows under any of its tops.
func TestMove(t *testing.T) {
	t.Parallel()
	tree, db := newTree(t, 0)
	n := add(t, tree, db, 6)
	a, b, c, d, e, f := n[0], n[1], n[2], n[3], n[4], n[5]
	for _, m := range [][2]int64{{b, a}, {c, b}, {d, c}, {e, a}} {
		_, err := move(tree, db, m[0], m[1])
		require.NoError(t, err)
	}
	path, err := tree.Ancestors(db, d)
	require.NoError(t, err)
	require.Equal(t, []int64{a, b, c}, path)
	require.Equal(t, []int64{b, c, d, e}, below(t, tree, db, a))
	require.Equal(t, []int64{c, d}, below(t, tree, db, b))
	require.Equal(t, []int64{c, d}, below(t, tree, db, b, e, a+1000))
	require.Empty(t, below(t, tree, db))
	require.Empty(t, below(t, tree, db, d))

	// b, with c and d below it, moves under f: a new tree.
	path, err = move(tree, db, b, f)
	require.NoError(t, err)
	require.Equal(t, []int64{f}, path)
	path, err = tree.Ancestors(db, d)
	require.NoError(t, err)
	require.Equal(t, []int64{f, b, c}, path)
	require.Equal(t, []int64{e}, below(t, tree, db, a))
	require.Equal(t, []int64{b, c, d}, below(t, tree, db, f))
	consistent(t, tree, db)

	// c becomes a root; d follows it.
	path, err = move(tree, db, c, 0)
	require.NoError(t, err)
	require.Empty(t, path)
	path, err = tree.Ancestors(db, d)
	require.NoError(t, err)
	require.Equal(t, []int64{c}, path)
	var root bool
	require.NoError(t, db.Table(tree.Table).Where("id = ?", c).Select("parent_id IS NULL").Scan(&root).Error)
	require.True(t, root)
	consistent(t, tree, db)

	_, err = tree.Ancestors(db, f+1000)
	require.ErrorIs(t, err, hierarchy.ErrNotFound)
}

// Move refuses a row under itself or under one below it, a row or parent
// that is not there, a tree past MaxDepth (the rows below the moved one
// counted), and a handle outside a transaction; a refused move changes
// nothing.
func TestMoveRefusals(t *testing.T) {
	t.Parallel()
	tree, db := newTree(t, 3)
	n := add(t, tree, db, 7)
	a, b, c, d, x, y, z := n[0], n[1], n[2], n[3], n[4], n[5], n[6]
	for _, m := range [][2]int64{{b, a}, {c, b}, {d, c}, {y, x}} { // a > b > c > d is 3 deep
		_, err := move(tree, db, m[0], m[1])
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		id, parent int64
		want       hierarchy.Error
	}{
		{b, b, hierarchy.ErrSelf},
		{a, d, hierarchy.ErrCycle},
		{b, c, hierarchy.ErrCycle},
		{z, d, hierarchy.ErrTooDeep},
		{x, c, hierarchy.ErrTooDeep}, // y would be 4 deep
		{z + 1000, a, hierarchy.ErrNotFound},
		{z, z + 1000, hierarchy.ErrNotFound},
	} {
		_, err := move(tree, db, tc.id, tc.parent)
		require.ErrorIs(t, err, tc.want, "%d under %d", tc.id, tc.parent)
		var code hierarchy.Error
		require.True(t, errors.As(err, &code))
		require.Equal(t, string(tc.want), code.Code())
	}
	_, err := move(tree, db, x, b)
	require.NoError(t, err, "y is 3 deep")
	consistent(t, tree, db)

	_, err = tree.Move(db, z, a)
	require.ErrorContains(t, err, "transaction")
}

// Two moves that would make a cycle together lock a tree in common: one
// waits for the other, sees it, and is refused.
func TestMoveConcurrentCycle(t *testing.T) {
	t.Parallel()
	tree, db := newTree(t, 0)
	n := add(t, tree, db, 4)
	a, a1, b, b1 := n[0], n[1], n[2], n[3]
	for _, m := range [][2]int64{{a1, a}, {b1, b}} {
		_, err := move(tree, db, m[0], m[1])
		require.NoError(t, err)
	}
	// a under b1 and b under a1, each holding its lock while the other
	// starts.
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, m := range [][2]int64{{a, b1}, {b, a1}} {
		go func() {
			errs <- db.Transaction(func(tx *gorm.DB) error {
				<-start
				if _, err := tree.Move(tx, m[0], m[1]); err != nil {
					return err
				}
				time.Sleep(50 * time.Millisecond)
				return nil
			})
		}()
	}
	close(start)
	var got []error
	for range 2 {
		got = append(got, <-errs)
	}
	require.Equal(t, 1, slices.IndexFunc(got, func(err error) bool { return err == nil })+
		slices.IndexFunc(got, func(err error) bool { return errors.Is(err, hierarchy.ErrCycle) }), "%v", got)
	consistent(t, tree, db)
}

// Concurrent moves across a few trees leave every path its parent's. A
// move that waits for one that re-roots its tree, and then for a third,
// may be chosen as a deadlock's victim: it fails, and changes nothing.
func TestMoveConcurrent(t *testing.T) {
	t.Parallel()
	tree, db := newTree(t, 6)
	ids := add(t, tree, db, 24)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for range 40 {
				id, parent := ids[r.IntN(len(ids))], int64(0)
				if r.IntN(4) > 0 {
					parent = ids[r.IntN(len(ids))]
				}
				_, err := move(tree, db, id, parent)
				var refused hierarchy.Error
				if err != nil && !errors.As(err, &refused) && !strings.Contains(err.Error(), "40P01") {
					t.Errorf("%d under %d: %v", id, parent, err)
				}
			}
		})
	}
	wg.Wait()
	consistent(t, tree, db)
	var deepest int
	require.NoError(t, db.Table(tree.Table).Select("COALESCE(max(cardinality(path)), 0)").Scan(&deepest).Error)
	require.LessOrEqual(t, deepest, 6)
}
