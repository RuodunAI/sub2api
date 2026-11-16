// Package settingswitch is a site-wide mode that the super admin flips at
// run time: session.issue (CONTRACTS §100.5).
package settingswitch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Switch is one field of one settings row, changed only by the super admin
// (the route adds settings:manage and a step-up proof), audited in the
// transaction that stores it and announced on config:changed; an
// environment variable pins it on a process. The owner keeps what the mode
// drives and how quickly it follows a change.
type Switch struct {
	Key     string   // the settings row; config:changed carries {"key":"settings.<Key>"}
	Field   string   // the field of the row holding the mode
	Env     string   // pins the mode on this process
	Values  []string // the modes
	Default string   // a missing, empty or unknown value
	Action  string   // the audit action of a change
	Log     string   // the log prefix of the owner ("authz", "iam")
	// Denied is the message of a change by anyone but the super admin.
	Denied core.LocalizedText
}

// IsSuperuser reports, through q, whether userID is the super admin.
type IsSuperuser func(ctx context.Context, q *gorm.DB, userID int64) (bool, error)

// Parse is v if it is a mode, else the default.
func (w Switch) Parse(v string) string {
	if slices.Contains(w.Values, v) {
		return v
	}
	return w.Default
}

// Pinned is the mode Env sets on this process, if it is set; an unknown
// value is the default, with a warning.
func (w Switch) Pinned() (string, bool) {
	raw, ok := os.LookupEnv(w.Env)
	if !ok || raw == "" {
		return "", false
	}
	if !slices.Contains(w.Values, raw) {
		slog.Warn(w.Log+": invalid "+w.Env+", using "+w.Default, "value", raw)
	}
	return w.Parse(raw), true
}

// Stored reads the mode as stored, "" when the row or field is missing (or
// null). A field that is not a string is an error, so that the owner keeps
// the mode it has instead of falling back to the default.
func (w Switch) Stored(ctx context.Context, db *gorm.DB) (string, error) {
	raw, err := store.SettingValue(store.G(ctx, db), w.Key)
	if store.IsNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	field, ok := doc[w.Field]
	if !ok {
		return "", nil
	}
	var v *string
	if err := json.Unmarshal(field, &v); err != nil {
		return "", fmt.Errorf("settings %q: %s is not a string: %w", w.Key, w.Field, err)
	}
	if v == nil {
		return "", nil
	}
	return *v, nil
}

// Source says where the mode a process uses comes from: env, setting or
// default.
func (w Switch) Source(pinned bool, stored string) string {
	switch {
	case pinned:
		return "env"
	case stored != "":
		return "setting"
	}
	return "default"
}

// Changed reports whether a config:changed payload announces this switch.
func (w Switch) Changed(payload []byte) bool {
	var event struct {
		Key string `json:"key"`
	}
	return json.Unmarshal(payload, &event) == nil && event.Key == "settings."+w.Key
}

// Set stores mode v, as actorID, and tells every node through bus (nil: no
// other node). Only the super admin may, as isSuperuser answers; the change
// is audited (w.Action: from, to, request_id) in the transaction that
// stores it. The caller applies it on this node.
func (w Switch) Set(ctx context.Context, db *store.DB, bus core.Bus, isSuperuser IsSuperuser, actorID int64, v string) error {
	if !slices.Contains(w.Values, v) {
		last := len(w.Values) - 1
		return core.InvalidFields(core.FieldError{Field: w.Field, Code: "invalid", Message: core.Text(ctx,
			"must be "+strings.Join(w.Values[:last], ", ")+" or "+w.Values[last],
			"必须是 "+strings.Join(w.Values[:last], "、")+" 或 "+w.Values[last])})
	}
	err := db.Tx(ctx, func(tx *gorm.DB) error {
		q := store.G(ctx, tx)
		su, err := isSuperuser(ctx, q, actorID)
		if err != nil {
			return err
		}
		if !su {
			return core.ErrPermissionDenied.WithMessage(core.Text(ctx, w.Denied["en"], w.Denied["zh"]))
		}
		var from string
		if _, err := store.UpdateSettingJSONTx(ctx, tx, w.Key, &actorID, func(raw json.RawMessage) (json.RawMessage, error) {
			return withField(raw, w.Field, v, &from)
		}); err != nil {
			return err
		}
		return audit.Audit(ctx, q, actorID, w.Action, "setting", w.Key,
			map[string]any{"from": from, "to": v, "request_id": core.RequestID(ctx)})
	})
	if err != nil {
		return err
	}
	if bus != nil {
		payload, _ := json.Marshal(map[string]string{"key": "settings." + w.Key})
		if err := bus.Publish(context.WithoutCancel(ctx), core.ChannelConfigChanged, payload); err != nil {
			slog.WarnContext(ctx, w.Log+": publish settings change failed", "key", w.Key, "err", err)
		}
	}
	return nil
}

// withField is the settings document raw with field set to v (other fields
// kept); from receives the string it held.
func withField(raw json.RawMessage, field, v string, from *string) (json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if old, ok := doc[field]; ok {
		_ = json.Unmarshal(old, from) // a non-string is audited as ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	doc[field] = b
	return json.Marshal(doc)
}
