package account

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The accounts columns with a non-zero default (schedulable, auto_disable,
// priority, max_concurrency, status) are pointers on the GORM path: a false
// or 0 the caller sends must reach the row on create and on PATCH, never
// fall back to the default.
func TestAccountZeroValuesAreWritten(t *testing.T) {
	e := setup(t)
	type columns struct {
		schedulable, autoDisable bool
		priority, maxConcurrency int
		status                   string
	}
	read := func(id int64) columns {
		t.Helper()
		var c columns
		require.NoError(t, e.db.SQL.QueryRowContext(context.Background(),
			`SELECT schedulable, auto_disable, priority, max_concurrency, status FROM accounts WHERE id = $1`, id).
			Scan(&c.schedulable, &c.autoDisable, &c.priority, &c.maxConcurrency, &c.status))
		return c
	}

	code, out := e.do("POST", "/accounts", map[string]any{"name": "zeros", "plugin_key": "anthropic", "type": "apikey",
		"credentials": map[string]any{"api_key": "sk-good-key-123"},
		"schedulable": false, "auto_disable": false, "priority": 0, "max_concurrency": 0, "status": "disabled"})
	require.Equal(t, 201, code, fmt.Sprint(out))
	id := int64(data(out)["id"].(float64))
	require.Equal(t, columns{priority: 0, maxConcurrency: 0, status: "disabled"}, read(id))

	// Back to non-zero values, then each to its zero by PATCH.
	code, out = e.do("PATCH", fmt.Sprintf("/accounts/%d", id), map[string]any{
		"schedulable": true, "auto_disable": true, "priority": 5, "max_concurrency": 3, "status": "active"})
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.Equal(t, columns{schedulable: true, autoDisable: true, priority: 5, maxConcurrency: 3, status: "active"}, read(id))
	code, out = e.do("PATCH", fmt.Sprintf("/accounts/%d", id), map[string]any{
		"schedulable": false, "auto_disable": false, "priority": 0, "max_concurrency": 0})
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.Equal(t, columns{status: "active"}, read(id))

	// A PATCH without them keeps them.
	code, out = e.do("PATCH", fmt.Sprintf("/accounts/%d", id), map[string]any{"name": "zeros-2"})
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.Equal(t, columns{status: "active"}, read(id))

	// Omitted on create, they take the defaults.
	code, out = e.do("POST", "/accounts", map[string]any{"name": "defaults", "plugin_key": "anthropic", "type": "apikey",
		"credentials": map[string]any{"api_key": "sk-good-key-123"}})
	require.Equal(t, 201, code, fmt.Sprint(out))
	def := read(int64(data(out)["id"].(float64)))
	require.True(t, def.schedulable && def.autoDisable && def.status == "active", "%+v", def)
}
