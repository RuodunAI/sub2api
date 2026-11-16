package settingswitch

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

var testSwitch = Switch{
	Key: "test", Field: "mode", Env: "SUB2API_TEST_SWITCH",
	Values: []string{"a", "b", "c"}, Default: "b", Action: "test.mode.set", Log: "test",
	Denied: core.LocalizedText{"en": "no", "zh": "不行"},
}

// published records what Set announces.
type published []string

func (p *published) Publish(_ context.Context, channel string, payload []byte) error {
	*p = append(*p, channel+" "+string(payload))
	return nil
}

func (p *published) Subscribe(string, func([]byte)) func() { return func() {} }

func TestParseAndPinned(t *testing.T) {
	assert.Equal(t, "a", testSwitch.Parse("a"))
	assert.Equal(t, "b", testSwitch.Parse("A"))
	assert.Equal(t, "b", testSwitch.Parse(""))

	t.Setenv(testSwitch.Env, "")
	_, ok := testSwitch.Pinned()
	assert.False(t, ok)
	t.Setenv(testSwitch.Env, "c")
	v, ok := testSwitch.Pinned()
	assert.True(t, ok)
	assert.Equal(t, "c", v)
	t.Setenv(testSwitch.Env, "z")
	v, ok = testSwitch.Pinned()
	assert.True(t, ok)
	assert.Equal(t, "b", v)

	assert.Equal(t, "env", testSwitch.Source(true, "a"))
	assert.Equal(t, "setting", testSwitch.Source(false, "a"))
	assert.Equal(t, "default", testSwitch.Source(false, ""))
	assert.True(t, testSwitch.Changed([]byte(`{"key":"settings.test"}`)))
	assert.False(t, testSwitch.Changed([]byte(`{"key":"settings.authz"}`)))
	assert.False(t, testSwitch.Changed([]byte(`not json`)))
}

// Stored is "" when nothing is stored, and an error, not the default, when
// the field is not a string: the owner keeps its mode.
func TestStored(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	v, err := testSwitch.Stored(ctx, db.Gorm)
	require.NoError(t, err)
	assert.Empty(t, v)
	for doc, want := range map[string]string{`{}`: "", `{"mode":null}`: "", `{"mode":"c","other":1}`: "c"} {
		require.NoError(t, db.Gorm.Exec(`INSERT INTO settings (key, value) VALUES ('test', ?)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, doc).Error)
		v, err := testSwitch.Stored(ctx, db.Gorm)
		require.NoError(t, err, doc)
		assert.Equal(t, want, v, doc)
	}
	for _, doc := range []string{`{"mode":3}`, `{"mode":["a"]}`, `[]`} {
		require.NoError(t, db.Gorm.Exec(`UPDATE settings SET value = ? WHERE key = 'test'`, doc).Error)
		_, err := testSwitch.Stored(ctx, db.Gorm)
		assert.Error(t, err, doc)
	}
}

func TestSet(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	actor := testutil.InsertUser(t, db, "root@x.com")
	superuser := func(su bool, err error) IsSuperuser {
		return func(context.Context, *gorm.DB, int64) (bool, error) { return su, err }
	}
	var bus published

	err := testSwitch.Set(ctx, db, &bus, superuser(true, nil), actor, "z")
	var fe *core.Error
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, []core.FieldError{{Field: "mode", Code: "invalid", Message: "must be a, b or c"}}, fe.Details["fields"])
	require.ErrorAs(t, testSwitch.Set(ctx, db, &bus, superuser(false, nil), actor, "a"), &fe)
	assert.Equal(t, core.ErrPermissionDenied.Code, fe.Code)
	assert.Equal(t, "no", fe.Message)
	boom := errors.New("boom")
	assert.ErrorIs(t, testSwitch.Set(ctx, db, &bus, superuser(false, boom), actor, "a"), boom)
	assert.Empty(t, bus)

	require.NoError(t, db.Gorm.Exec(`INSERT INTO settings (key, value) VALUES ('test', '{"mode":"c","other":1}')`).Error)
	require.NoError(t, testSwitch.Set(ctx, db, &bus, superuser(true, nil), actor, "a"))
	var doc string
	require.NoError(t, db.Gorm.Raw(`SELECT value::text FROM settings WHERE key = 'test'`).Row().Scan(&doc))
	assert.JSONEq(t, `{"mode":"a","other":1}`, doc)
	var audited int
	require.NoError(t, db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'test.mode.set' AND user_id = ?
		AND target_id = 'test' AND detail->>'from' = 'c' AND detail->>'to' = 'a'`, actor).Row().Scan(&audited))
	assert.Equal(t, 1, audited)
	assert.Equal(t, published{core.ChannelConfigChanged + ` {"key":"settings.test"}`}, bus)
}
