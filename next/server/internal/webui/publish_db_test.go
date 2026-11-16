package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Publish writes every file of a build in one statement: each blob with
// its hash and size (an empty file included), each path listed among the
// build's files, and publishing again (same bytes) changes nothing.
func TestPublishStoresEveryFile(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	files := map[string]string{
		"assets/a-abcdefgh.js":  "console.log(1)",
		"assets/b-12345678.css": "",
		"assets/c-87654321.svg": "<svg/>",
	}
	build := fstest.MapFS{"dist/index.html": {Data: []byte(`<html></html>`)}}
	for name, body := range files {
		build["dist/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	s, err := NewSharedAssets(db, build, SharedOptions{})
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, s.Publish(ctx))
		rows, err := db.SQL.QueryContext(ctx, `SELECT a.path, a.sha256, a.body, a.size, f.build_id
			FROM web_assets a JOIN web_asset_build_files f ON f.path = a.path ORDER BY a.path`)
		require.NoError(t, err)
		type stored struct {
			path, hash string
			body       []byte
			size       int64
			build      string
		}
		var got []stored
		for rows.Next() {
			var st stored
			require.NoError(t, rows.Scan(&st.path, &st.hash, &st.body, &st.size, &st.build))
			got = append(got, st)
		}
		require.NoError(t, rows.Err())
		rows.Close()
		require.Len(t, got, len(files))
		for _, st := range got {
			body, ok := files[st.path]
			require.True(t, ok, st.path)
			sum := sha256.Sum256([]byte(body))
			require.Equal(t, hex.EncodeToString(sum[:]), st.hash, st.path)
			require.Equal(t, body, string(st.body), st.path)
			require.NotNil(t, st.body, "an empty file is an empty body, not NULL")
			require.EqualValues(t, len(body), st.size, st.path)
			require.Equal(t, s.buildID, st.build, st.path)
		}
	}
}
