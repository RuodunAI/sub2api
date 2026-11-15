// Package event is the write side of the transactional outbox (table
// events). Delivery to plugins lives in event/delivery (owner: H).
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Publisher implements core.EventPublisher.
type Publisher struct {
	db *store.DB
}

var _ core.EventPublisher = (*Publisher)(nil)

// NewPublisher returns an outbox writer. db is used only when Emit is called
// without a transaction.
func NewPublisher(db *store.DB) *Publisher { return &Publisher{db: db} }

// Emit inserts events into the outbox inside tx. With a nil tx the events
// are written in their own transaction (only for callers that have no
// business transaction to join).
func (p *Publisher) Emit(ctx context.Context, tx *gorm.DB, events ...core.Event) error {
	if len(events) == 0 {
		return nil
	}
	if tx == nil {
		if p.db == nil {
			return errors.New("event: Emit without transaction and no database")
		}
		return p.db.Tx(ctx, func(tx *gorm.DB) error { return p.Emit(ctx, tx, events...) })
	}
	db := store.G(ctx, tx)
	if len(events) == 1 {
		payload, err := eventPayload(events[0])
		if err != nil {
			return err
		}
		return insertEvent(db, events[0].Type, payload).Error
	}
	types, payloads := make(store.Strings, len(events)), make(store.Array[store.Bytes], len(events))
	for i, ev := range events {
		payload, err := eventPayload(ev)
		if err != nil {
			return err
		}
		types[i], payloads[i] = ev.Type, payload
	}
	return insertEvents(db, types, payloads).Error
}

// eventPayload checks ev and returns its payload document.
func eventPayload(ev core.Event) ([]byte, error) {
	if ev.Type == "" {
		return nil, errors.New("event: empty type")
	}
	payload, err := marshal(ev.Payload)
	if err != nil {
		return nil, fmt.Errorf("event %s: %w", ev.Type, err)
	}
	return payload, nil
}

// insertEventsSQL inserts any number of events, one row each in their order
// (the ids follow it), in one statement and one round trip: the types and
// the payloads are an array parameter each, which unnest turns back into
// rows, numbered WITH ORDINALITY and inserted in that order. The text is
// the same for every count, so a connection prepares it once; a VALUES list
// of the events would be a new text, and a Prepare round trip, for each new
// count, in the caller's transaction, which may hold a user's balance lock
// (usage insertAtomic), and would bind two parameters per event, of
// PostgreSQL's 65535. The elements are the values a VALUES row bound: the
// type as varchar, the payload document as its text.
const insertEventsSQL = `INSERT INTO events (type, payload)
	SELECT t, p FROM unnest(?::varchar[], ?::jsonb[]) WITH ORDINALITY AS e(t, p, o) ORDER BY o`

// insertEventSQL inserts one event, the usual count, with its values bound
// as they are: one fixed text as well, and cheaper than two one-element
// arrays (gormtest/BASELINE.md).
const insertEventSQL = `INSERT INTO events (type, payload) VALUES (?, ?)`

// insertEvent runs insertEventSQL on db.
func insertEvent(db *gorm.DB, typ string, payload []byte) *gorm.DB {
	return db.Exec(insertEventSQL, typ, payload)
}

// insertEvents runs insertEventsSQL for the events of types and payloads
// (one of each per event) on db.
func insertEvents(db *gorm.DB, types store.Strings, payloads store.Array[store.Bytes]) *gorm.DB {
	return db.Exec(insertEventsSQL, types, payloads)
}

func marshal(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("{}"), nil
	case json.RawMessage:
		if !json.Valid(x) {
			return nil, errors.New("invalid JSON payload")
		}
		return x, nil
	case []byte:
		if !json.Valid(x) {
			return nil, errors.New("invalid JSON payload")
		}
		return x, nil
	}
	return json.Marshal(v)
}
