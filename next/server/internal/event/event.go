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
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
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
	rows := make([]eventRow, len(events))
	for i, ev := range events {
		if ev.Type == "" {
			return errors.New("event: empty type")
		}
		payload, err := marshal(ev.Payload)
		if err != nil {
			return fmt.Errorf("event %s: %w", ev.Type, err)
		}
		rows[i] = eventRow{Type: ev.Type, Payload: payload}
	}
	return insertEvents(store.G(ctx, tx), rows)
}

// insertBatch is the most events one INSERT writes: two parameters each,
// well under PostgreSQL's 65535.
const insertBatch = 1000

// eventRow is the part of an outbox row that Emit writes. The id and
// occurred_at come from the column defaults (the sequence, now()): the
// struct has no field for them, so GORM neither binds nor reads them back
// (no RETURNING), and as a typed model its schema is parsed once and cached
// (a []map[string]any destination fails the parse on every insert and
// formats the rows into the discarded error).
type eventRow struct {
	Type    string `gorm:"column:type"`
	Payload []byte `gorm:"column:payload"`
}

func (eventRow) TableName() string { return model.Event{}.TableName() }

// insertEvents writes rows to the outbox in one INSERT per insertBatch
// events, inside the caller's transaction and with no round trip of its
// own. The ids follow the batch order: each row takes the next id as the
// VALUES list hands it over, in its order, as the rows of the former
// unnest WITH ORDINALITY ... ORDER BY did. The usual few events go through
// Create: CreateInBatches clones the session and the slice even for one
// batch, on every request that emits.
func insertEvents(db *gorm.DB, rows []eventRow) error {
	if len(rows) <= insertBatch {
		return db.Create(&rows).Error
	}
	return db.CreateInBatches(&rows, insertBatch).Error
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
