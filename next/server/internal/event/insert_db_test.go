package event_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// storedEvents is the type and payload text of the events after id, in id
// order.
func storedEvents(t *testing.T, db *store.DB, after int64) []string {
	t.Helper()
	rows, err := db.SQL.QueryContext(context.Background(), `SELECT type || ' ' || payload::text FROM events WHERE id > $1 ORDER BY id`, after)
	require.NoError(t, err)
	got, err := testutil.Column[string](rows)
	require.NoError(t, err)
	return got
}

func lastEventID(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.SQL.QueryRowContext(context.Background(), `SELECT COALESCE(max(id), 0) FROM events`).Scan(&id))
	return id
}

// Several events (one INSERT) are stored as each on its own
// (one row) stores it: in their order, every kind of payload as
// the same document, nil as {} and a JSON null as a document, not NULL.
func TestEmitManyAsOne(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	pub := event.NewPublisher(db)
	events := []core.Event{
		{Type: core.EventBalanceChanged, Payload: map[string]any{"user_id": 7, "delta": "0.5"}},
		{Type: "x.nil", Payload: nil},
		{Type: "x.null", Payload: json.RawMessage(`null`)},
		{Type: "x.bytes", Payload: []byte(` [1, 2] `)},
		{Type: "x.string", Payload: "a \"quoted\" \\ text"},
		{Type: "x.number", Payload: 1.5},
		{Type: "x.unicode", Payload: json.RawMessage(`{"k":"é😀","z":{}}`)},
	}
	want := []string{
		`balance.changed {"delta": "0.5", "user_id": 7}`, `x.nil {}`, `x.null null`, `x.bytes [1, 2]`,
		`x.string "a \"quoted\" \\ text"`, `x.number 1.5`, `x.unicode {"k": "é😀", "z": {}}`,
	}

	start := lastEventID(t, db)
	require.NoError(t, pub.Emit(ctx, nil, events...))
	require.Equal(t, want, storedEvents(t, db, start))

	start = lastEventID(t, db)
	for _, ev := range events {
		require.NoError(t, pub.Emit(ctx, nil, ev))
	}
	require.Equal(t, want, storedEvents(t, db, start), "one at a time")

	// Invalid and empty raw payloads fail either way, and write nothing.
	start = lastEventID(t, db)
	for _, bad := range []any{json.RawMessage(``), json.RawMessage(`{`), []byte(nil)} {
		require.Error(t, pub.Emit(ctx, nil, core.Event{Type: "x.bad", Payload: bad}), "%#v", bad)
		require.Error(t, pub.Emit(ctx, nil, core.Event{Type: "x.ok", Payload: 1}, core.Event{Type: "x.bad", Payload: bad}), "%#v", bad)
	}
	require.Error(t, pub.Emit(ctx, nil, core.Event{Type: "x.ok"}, core.Event{Type: ""}))
	require.Empty(t, storedEvents(t, db, start))
}

// More events than one INSERT could bind (two parameters each, 65535 in
// all) are written in batches, the ids in event order across them.
func TestEmitPastParameterLimit(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	const n = 40000
	events := make([]core.Event, n)
	for i := range events {
		events[i] = core.Event{Type: fmt.Sprintf("x.%d", i), Payload: map[string]int{"i": i}}
	}
	start := lastEventID(t, db)
	require.NoError(t, event.NewPublisher(db).Emit(ctx, nil, events...))
	got := storedEvents(t, db, start)
	require.Len(t, got, n)
	for i, g := range got {
		if want := fmt.Sprintf(`x.%d {"i": %d}`, i, i); g != want {
			t.Fatalf("event %d = %q, want %q", i, g, want)
		}
	}
}
