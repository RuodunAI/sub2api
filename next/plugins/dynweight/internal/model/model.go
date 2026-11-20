// Package model describes the tables of dynweight as GORM models: the one
// description of them. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
package model

import (
	"encoding/json"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// All lists every model, for sub2api-plugin db.
func All() []any {
	return []any{&Config{}, &Obs{}, &State{}, &Base{}, &Event{}, &StickyPending{}}
}

// Prefix starts the name of every table, index and sequence of the plugin:
// they live in the core's schema (manifest.TablePrefix, CONTRACTS §130).
const Prefix = "plg_dynweight__"

// ConfigID is the id of the one configuration row.
const ConfigID = 1

// Config holds the configuration document (rules.Config), one row.
type Config struct {
	ID        int16           `gorm:"primaryKey;autoIncrement:false;check:plg_dynweight__config_id_check,id = 1"`
	Doc       json.RawMessage `gorm:"type:jsonb;not null"`
	Version   int64           `gorm:"not null"`
	UpdatedAt time.Time       `gorm:"not null;default:now();autoUpdateTime:false"`
	UpdatedBy int64           `gorm:"not null;default:0"`
}

func (Config) TableName() string { return Prefix + "config" }

// Obs holds the attempt counters of an (account, group, client model) in
// one window, summed by every node (rules.Counts fields).
type Obs struct {
	AccountID   int64           `gorm:"primaryKey;autoIncrement:false"`
	GroupID     int64           `gorm:"primaryKey;autoIncrement:false"`
	Model       string          `gorm:"primaryKey"`
	WindowStart int64           `gorm:"primaryKey;autoIncrement:false;index:plg_dynweight__obs_window_start_idx"` // unix seconds
	Counts      json.RawMessage `gorm:"type:jsonb;not null"`
}

func (Obs) TableName() string { return Prefix + "obs" }

// State is the evaluation state of an (account, group, client model)
// (rules.State). Base, Eff and Tombstone repeat the document for the
// scheduling snapshot; Gen guards the write-back against a concurrent reset.
type State struct {
	AccountID  int64           `gorm:"primaryKey;autoIncrement:false;index:plg_dynweight__state_changed_idx,where:NOT tombstone AND eff <> base"`
	GroupID    int64           `gorm:"primaryKey;autoIncrement:false"`
	Model      string          `gorm:"primaryKey"`
	Data       json.RawMessage `gorm:"type:jsonb;not null"`
	Base       int             `gorm:"type:integer;not null"`
	Eff        float64         `gorm:"type:double precision;not null"`
	Tombstone  bool            `gorm:"not null;default:false"`
	EvalWindow int64           `gorm:"not null"`
	Gen        int64           `gorm:"not null;default:1"`
	UpdatedAt  time.Time       `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (State) TableName() string { return Prefix + "state" }

// Base is the base weight scheduling handed in for an (account, group):
// the evaluation runs outside requests and reads it here.
type Base struct {
	AccountID int64     `gorm:"primaryKey;autoIncrement:false"`
	GroupID   int64     `gorm:"primaryKey;autoIncrement:false"`
	Weight    int       `gorm:"type:integer;not null"`
	Name      string    `gorm:"not null;default:''"`
	SeenAt    time.Time `gorm:"not null;default:now()"`
}

func (Base) TableName() string { return Prefix + "bases" }

// Event is what an evaluation did (rules.Event), or a manual reset.
type Event struct {
	ID         int64              `gorm:"primaryKey"`
	CreatedAt  time.Time          `gorm:"not null;default:now();autoCreateTime:false;index:plg_dynweight__events_created_at_idx;index:plg_dynweight__events_account_idx,priority:2"`
	AccountID  int64              `gorm:"not null;index:plg_dynweight__events_account_idx,priority:1"`
	GroupID    int64              `gorm:"not null"`
	Model      string             `gorm:"not null"`
	Kind       string             `gorm:"not null"`
	Reason     string             `gorm:"not null;default:''"`
	WeightFrom float64            `gorm:"type:double precision;not null;default:0"`
	WeightTo   float64            `gorm:"type:double precision;not null;default:0"`
	Rules      pgdb.Array[string] `gorm:"type:text[];not null;default:'{}'"`
	Detail     json.RawMessage    `gorm:"type:jsonb;not null;default:'{}'"`
}

func (Event) TableName() string { return Prefix + "events" }

// StickyPending is a sticky clear an evaluation decided and the core has not
// done yet, one per (account, group, client model). Salt is the window
// start; ClaimedUntil leases the row to one run ('-infinity': free).
type StickyPending struct {
	AccountID    int64     `gorm:"primaryKey;autoIncrement:false"`
	GroupID      int64     `gorm:"primaryKey;autoIncrement:false"`
	Model        string    `gorm:"primaryKey"`
	Salt         string    `gorm:"not null"`
	Percent      int       `gorm:"type:integer;not null"`
	Strategies   string    `gorm:"not null;default:''"`
	Zero         bool      `gorm:"not null;default:false"`
	Weight       float64   `gorm:"type:double precision;not null;default:0"`
	Attempts     int       `gorm:"type:integer;not null;default:0"`
	CreatedAt    time.Time `gorm:"not null;default:now();autoCreateTime:false;index:plg_dynweight__sticky_pending_created_idx"`
	ClaimedUntil time.Time `gorm:"not null;default:'-infinity'"`
}

func (StickyPending) TableName() string { return Prefix + "sticky_pending" }
