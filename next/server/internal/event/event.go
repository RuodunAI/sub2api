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
	rows := make([]model.Event, len(events))
	for i, ev := range events {
		if ev.Type == "" {
			return errors.New("event: empty type")
		}
		payload, err := marshal(ev.Payload)
		if err != nil {
			return fmt.Errorf("event %s: %w", ev.Type, err)
		}
		rows[i] = model.Event{Type: ev.Type, Payload: payload}
	}
	return insertEvents(store.G(ctx, tx), rows)
}

// insertBatch is the most events one INSERT writes: three parameters each,
// well under PostgreSQL's 65535.
const insertBatch = 1000

// insertEvents writes rows to the outbox, their ids ascending in their
// order. One event, the usual count, takes the next id as it is written and
// reads nothing back (a column map: no RETURNING). More reserve theirs
// first (store.NextIDs), so the ids follow the batch order whatever order
// the INSERT stores the rows in.
func insertEvents(db *gorm.DB, rows []model.Event) error {
	if len(rows) == 1 {
		return db.Table(model.Event{}.TableName()).Create(map[string]any{"type": rows[0].Type, "payload": rows[0].Payload}).Error
	}
	ids, err := store.NextIDs(db, model.Event{}.TableName(), len(rows))
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].ID = ids[i]
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
