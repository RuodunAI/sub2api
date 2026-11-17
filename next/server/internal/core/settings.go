package core

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// System settings as plugins read them (CONTRACTS §106). Each section has
// one owner (billing, gateway, environment) that reads it with the same
// load and normalisation as its own cache and hands it out as the SDK type
// of the section (protocol.BillingSettings ...). The owner records
// settings.changed in the transaction that saves a section and broadcasts
// config:changed {"key":"settings.<section>"} after the commit; on every
// node the plugin runtime turns that broadcast into
// PluginService.SettingsChanged.

// EventSettingsChanged is the outbox event of a saved section.
const EventSettingsChanged = protocol.EventSettingsChanged

// SettingsReader reads one section; the owner's database errors are
// returned as they are.
type SettingsReader func(ctx context.Context) (protocol.SettingsValues, error)

// EmitSettingsChanged records settings.changed for section in tx, the
// transaction that saved it. A nil events records nothing (tests).
func EmitSettingsChanged(ctx context.Context, events EventPublisher, tx *gorm.DB, section string) error {
	if events == nil {
		return nil
	}
	return events.Emit(ctx, tx, Event{Type: EventSettingsChanged, Payload: protocol.SettingsChange{Sections: []string{section}}})
}
