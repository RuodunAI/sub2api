// Package model is the schema of plg_guard as GORM models: the one
// description of its tables. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
package model

import "time"

// All lists every model, for sub2api-plugin db.
func All() []any {
	return []any{&Rule{}, &BlockLog{}, &StatsMinutely{}, &StatsHourly{}, &ProcessedEvent{}}
}

// Rule is one blocking rule (kind keyword or regex).
type Rule struct {
	ID      int64  `gorm:"primaryKey"`
	Name    string `gorm:"type:varchar(100);not null"`
	Kind    string `gorm:"type:varchar(20);not null;check:rules_kind_check,kind IN ('keyword', 'regex')"`
	Pattern string `gorm:"not null"`
	// GORM writes the default for a zero field it creates from a struct:
	// create a disabled rule from a map.
	Enabled   bool      `gorm:"not null;default:true"`
	CreatedAt time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
}

func (Rule) TableName() string { return "rules" }

// BlockLog is one blocked request; Snippet only with record_snippets on.
type BlockLog struct {
	ID         int64     `gorm:"primaryKey"`
	OccurredAt time.Time `gorm:"not null;index:block_log_occurred_at_idx"`
	RuleID     int64     `gorm:"not null"`
	RuleName   string    `gorm:"type:varchar(100);not null"`
	RequestID  string    `gorm:"type:varchar(100);not null;default:''"`
	UserID     int64     `gorm:"not null;default:0"`
	GroupID    int64     `gorm:"not null;default:0"`
	Model      string    `gorm:"type:varchar(200);not null;default:''"`
	Snippet    *string
}

func (BlockLog) TableName() string { return "block_log" }

// StatsMinutely counts the requests (usage.recorded events) and blocks
// (the hook) of a group and model in a minute.
type StatsMinutely struct {
	Minute   time.Time `gorm:"primaryKey"`
	GroupID  int64     `gorm:"primaryKey;autoIncrement:false"`
	Model    string    `gorm:"primaryKey;type:varchar(200)"`
	Requests int64     `gorm:"not null;default:0"`
	Blocked  int64     `gorm:"not null;default:0"`
}

func (StatsMinutely) TableName() string { return "stats_minutely" }

// StatsHourly is the rollup of StatsMinutely by hour (UTC).
type StatsHourly struct {
	Hour     time.Time `gorm:"primaryKey"`
	GroupID  int64     `gorm:"primaryKey;autoIncrement:false"`
	Model    string    `gorm:"primaryKey;type:varchar(200)"`
	Requests int64     `gorm:"not null;default:0"`
	Blocked  int64     `gorm:"not null;default:0"`
}

func (StatsHourly) TableName() string { return "stats_hourly" }

// ProcessedEvent is an event id already applied (at-least-once delivery).
type ProcessedEvent struct {
	EventID     int64     `gorm:"primaryKey;autoIncrement:false"`
	ProcessedAt time.Time `gorm:"not null;default:now();index:processed_events_processed_at_idx"`
}

func (ProcessedEvent) TableName() string { return "processed_events" }
