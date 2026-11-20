// Package model describes the tables of moderation as GORM models: the one
// description of them. migrations/*.sql stay what the host runs;
// `sub2api-plugin db diff` writes the next one from a change made here, and
// `sub2api-plugin db check` (run by pack) fails while the two disagree.
//
// The columns are spelled out as migrations 0001-0013 left them, CHECK
// constraints under the names PostgreSQL gave them. Timestamps the database
// fills keep GORM's automatic created_at/updated_at off.
package model

import (
	"encoding/json"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// Prefix starts the name of every table, index and sequence of the plugin:
// they live in the core's schema (manifest.TablePrefix, CONTRACTS §130).
const Prefix = "plg_moderation__"

// All lists every model, for sub2api-plugin db.
func All() []any {
	return []any{&Event{}, &Block{}, &Unblock{}, &BlockSubject{}, &Channel{}, &Auditor{}, &Flow{}, &FlowVersion{},
		&FlowGroup{}, &FlowAccount{}, &ConfigState{}, &Watch{}, &WatchEvent{}, &Cost{}, &StatsHourly{}, &UserDaily{}}
}

// Event is one moderation record. verdict, action and the other 0.1
// columns stay filled for 0.1 readers; the record of 0.2 on is result,
// timing, flow and trace.
type Event struct {
	ID int64 `gorm:"primaryKey"`
	// The record's time leads every time-ordered index.
	CreatedAt          time.Time          `gorm:"not null;default:now();autoCreateTime:false;index:plg_moderation__events_created_at_idx;index:plg_moderation__events_verdict_created_at_idx,priority:2;index:plg_moderation__events_user_created_at_idx,priority:2;index:plg_moderation__events_result_created_at_idx,priority:2;index:plg_moderation__events_session_fp_idx,priority:2,where:session_fp <> ''"`
	RequestID          string             `gorm:"not null;default:'';index:plg_moderation__events_request_id_idx"`
	UserID             int64              `gorm:"not null;default:0;index:plg_moderation__events_user_created_at_idx,priority:1"`
	APIKeyID           int64              `gorm:"column:api_key_id;not null;default:0"`
	GroupID            int64              `gorm:"not null;default:0"`
	Model              string             `gorm:"not null;default:''"`
	Protocol           string             `gorm:"not null;default:''"`
	Mode               string             `gorm:"not null;check:plg_moderation__events_mode_check,mode IN ('observe', 'enforce')"`
	Verdict            string             `gorm:"not null;check:plg_moderation__events_verdict_check,verdict IN ('pass', 'flag', 'block', 'error');index:plg_moderation__events_verdict_created_at_idx,priority:1"`
	Action             string             `gorm:"not null;check:plg_moderation__events_action_check,action IN ('allow', 'deny')"`
	Categories         pgdb.Array[string] `gorm:"type:text[];not null;default:'{}'"`
	Severity           string             `gorm:"not null;default:''"`
	Reason             string             `gorm:"not null;default:''"`
	Error              string             `gorm:"not null;default:''"`
	Text               *string            // NULL when store_text is off
	TextChars          int                `gorm:"type:integer;not null;default:0"`
	TextHash           string             `gorm:"not null;default:''"`
	Cached             bool               `gorm:"not null;default:false"`
	LLMModel           string             `gorm:"column:llm_model;not null;default:''"`
	Turns              int                `gorm:"type:integer;not null;default:0"`
	LatencyMs          int                `gorm:"type:integer;not null;default:0"`
	PromptTokens       int                `gorm:"type:integer;not null;default:0"`
	CompletionTokens   int                `gorm:"type:integer;not null;default:0"`
	Result             string             `gorm:"not null;default:'';index:plg_moderation__events_result_created_at_idx,priority:1"`
	Timing             string             `gorm:"not null;default:''"`
	FlowID             int64              `gorm:"not null;default:0"`
	FlowVersion        int                `gorm:"type:integer;not null;default:0"`
	HighestCategory    string             `gorm:"not null;default:''"`
	HighestScore       float64            `gorm:"type:double precision;not null;default:0"`
	HitCategories      pgdb.Array[string] `gorm:"type:text[];not null;default:'{}'"`
	Trace              json.RawMessage    `gorm:"type:jsonb"`
	InputFormat        string             `gorm:"not null;default:''"`
	InputMessage       *string
	InputMessageSize   int    `gorm:"type:integer;not null;default:0"`
	ImageCount         int    `gorm:"type:integer;not null;default:0"`
	ReviewStatus       string `gorm:"not null;default:'pending';check:plg_moderation__events_review_status_chk,review_status IN ('pending', 'false_positive', 'confirmed')"`
	ReviewedBy         *int64
	ReviewedAt         *time.Time
	ViolationEpoch     int64      `gorm:"not null;default:0"`
	Counted            bool       `gorm:"not null;default:false"`
	KeepUntil          *time.Time `gorm:"index:plg_moderation__events_keep_until_idx"`
	QueueMs            int        `gorm:"type:integer;not null;default:0"`
	Endpoint           string     `gorm:"not null;default:''"`
	SessionFp          string     `gorm:"not null;default:'';index:plg_moderation__events_session_fp_idx,priority:1,where:session_fp <> ''"`
	Replayed           bool       `gorm:"not null;default:false"`
	FlowSource         *string
	FlowFallbackID     *int64
	FlowFallbackReason *string
	Level              *string
	LevelSource        *string
}

func (Event) TableName() string { return Prefix + "events" }

// Block is a banned user (automatic after ban_threshold violations, or
// manual); ExpiresAt nil = until unblocked.
type Block struct {
	UserID     int64      `gorm:"primaryKey;autoIncrement:false"`
	Reason     string     `gorm:"not null;default:''"`
	Violations int        `gorm:"type:integer;not null;default:0"`
	Source     string     `gorm:"not null;check:plg_moderation__blocks_source_check,source IN ('auto', 'manual')"`
	CreatedAt  time.Time  `gorm:"not null;default:now();autoCreateTime:false"`
	ExpiresAt  *time.Time `gorm:"index:plg_moderation__blocks_expires_at_idx"`
	CreatedBy  *int64
}

func (Block) TableName() string { return Prefix + "blocks" }

// Unblock is the last unblock of a user: violations count after it only.
type Unblock struct {
	UserID int64     `gorm:"primaryKey;autoIncrement:false"`
	At     time.Time `gorm:"not null"`
}

func (Unblock) TableName() string { return Prefix + "unblocks" }

// BlockSubject is the durable parent row every ban and watch decision of a
// user locks first (lockBlockSubject); never deleted.
type BlockSubject struct {
	UserID int64 `gorm:"primaryKey;autoIncrement:false"`
}

func (BlockSubject) TableName() string { return Prefix + "block_subjects" }

// Channel is an upstream the auditors call; key and proxy are sealed with
// the host master key.
type Channel struct {
	ID           int64     `gorm:"primaryKey"`
	Type         string    `gorm:"not null;check:plg_moderation__channels_type_check,type IN ('openai_moderation', 'typesafe', 'llm')"`
	Protocol     string    `gorm:"not null;default:''"`
	Name         string    `gorm:"not null;default:''"`
	BaseURL      string    `gorm:"column:base_url;not null;default:''"`
	APIKeyEnc    []byte    `gorm:"column:api_key_enc;type:bytea"`
	APIKeyTail   string    `gorm:"column:api_key_tail;not null;default:''"`
	ProxyEnc     []byte    `gorm:"type:bytea"`
	Model        string    `gorm:"not null;default:''"`
	TimeoutMs    int       `gorm:"type:integer;not null;default:0"`
	Enabled      bool      `gorm:"not null;default:true"`
	CreatedAt    time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt    time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
	DeletedAt    *time.Time
	BillingModel string `gorm:"not null;default:''"`
}

func (Channel) TableName() string { return Prefix + "channels" }

// Auditor is a name, a bound channel and the judging parameters.
type Auditor struct {
	ID        int64           `gorm:"primaryKey"`
	Name      string          `gorm:"not null;default:''"`
	Type      string          `gorm:"not null;check:plg_moderation__auditors_type_check,type IN ('openai_moderation', 'typesafe', 'llm_verdict', 'llm_questions')"`
	ChannelID int64           `gorm:"not null"`
	Config    json.RawMessage `gorm:"type:jsonb;not null;default:'{}'"`
	CreatedAt time.Time       `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt time.Time       `gorm:"not null;default:now();autoUpdateTime:false"`
	DeletedAt *time.Time
}

func (Auditor) TableName() string { return Prefix + "auditors" }

// Flow is a draft and its published spec; every published spec is also a
// FlowVersion. Scope NULL: nobody chose one.
type Flow struct {
	ID          int64           `gorm:"primaryKey"`
	Name        string          `gorm:"not null;default:''"`
	Enabled     bool            `gorm:"not null;default:true"`
	Draft       json.RawMessage `gorm:"type:jsonb;not null;default:'{}'"`
	Published   json.RawMessage `gorm:"type:jsonb"`
	Version     int             `gorm:"type:integer;not null;default:0"`
	PublishedAt *time.Time
	PublishedBy *int64
	CreatedAt   time.Time `gorm:"not null;default:now();autoCreateTime:false"`
	UpdatedAt   time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
	DeletedAt   *time.Time
	Scope       *string
}

func (Flow) TableName() string { return Prefix + "flows" }

// FlowVersion is one published spec of a flow. AutoTimeout: published with
// total_timeout_ms 0, resolved in the spec.
type FlowVersion struct {
	FlowID      int64           `gorm:"primaryKey;autoIncrement:false"`
	Version     int             `gorm:"type:integer;primaryKey;autoIncrement:false"`
	Spec        json.RawMessage `gorm:"type:jsonb;not null"`
	PublishedAt time.Time       `gorm:"not null;default:now()"`
	PublishedBy *int64
	AutoTimeout bool `gorm:"not null;default:false"`
}

func (FlowVersion) TableName() string { return Prefix + "flow_versions" }

// FlowGroup binds a group (0 = the default flow) to a flow (0 = not
// moderated).
type FlowGroup struct {
	GroupID int64 `gorm:"primaryKey;autoIncrement:false"`
	FlowID  int64 `gorm:"not null;index:plg_moderation__flow_groups_flow_idx"`
}

func (FlowGroup) TableName() string { return Prefix + "flow_groups" }

// FlowAccount binds an account within a group (0 = in any group) to a flow
// (0 = not moderated).
type FlowAccount struct {
	GroupID   int64 `gorm:"primaryKey;autoIncrement:false;not null;default:0;check:plg_moderation__flow_accounts_group_id_check,group_id >= 0"`
	AccountID int64 `gorm:"primaryKey;autoIncrement:false;not null;check:plg_moderation__flow_accounts_account_id_check,account_id > 0"`
	FlowID    int64 `gorm:"not null;check:plg_moderation__flow_accounts_flow_id_check,flow_id >= 0;index:plg_moderation__flow_accounts_flow_idx"`
}

func (FlowAccount) TableName() string { return Prefix + "flow_accounts" }

// ConfigStateID is the id of the one config_state row (migration 0003
// inserts it).
const ConfigStateID = 1

// ConfigState holds the change counters the nodes poll and the one-time
// upgrade marks.
type ConfigState struct {
	ID                int32 `gorm:"type:integer;primaryKey;autoIncrement:false;check:plg_moderation__config_state_id_check,id = 1"`
	ConfigVersion     int64 `gorm:"not null;default:0"`
	WatchVersion      int64 `gorm:"not null;default:0"`
	LegacyConverted   bool  `gorm:"not null;default:false"`
	CacheEpoch        int64 `gorm:"not null;default:0"`
	ParityConverted   bool  `gorm:"not null;default:false"`
	ParityConvertedAt *time.Time
	ListsImported     bool `gorm:"not null;default:false"`
}

func (ConfigState) TableName() string { return Prefix + "config_state" }

// Watch is a watch list entry, one per user; a locked entry puts the user
// on a list.
type Watch struct {
	UserID         int64      `gorm:"primaryKey;autoIncrement:false"`
	Level          string     `gorm:"not null;check:plg_moderation__watchlist_level_check,level IN ('observe', 'enforce')"`
	Source         string     `gorm:"not null;check:plg_moderation__watchlist_source_check,source IN ('auto', 'manual', 'analysis')"`
	SinceAt        time.Time  `gorm:"not null;default:now()"`
	ExpiresAt      *time.Time `gorm:"index:plg_moderation__watchlist_expires_at_idx"`
	HitCount       int        `gorm:"type:integer;not null;default:0"`
	ViolationCount int        `gorm:"type:integer;not null;default:0"`
	ViolationEpoch int64      `gorm:"not null;default:0"`
	LastHitAt      *time.Time
	Note           string    `gorm:"not null;default:''"`
	UpdatedAt      time.Time `gorm:"not null;default:now();autoUpdateTime:false"`
	HitsSinceSeen  int       `gorm:"type:integer;not null;default:0"`
	Locked         bool      `gorm:"not null;default:false"`
}

func (Watch) TableName() string { return Prefix + "watchlist" }

// WatchEvent is one row of the watch history (operator nil = automatic).
// AnalysisID stays for the audit of changes the retired analysis made.
type WatchEvent struct {
	ID         int64  `gorm:"primaryKey;index:plg_moderation__watch_events_user_idx,priority:2"`
	UserID     int64  `gorm:"not null;index:plg_moderation__watch_events_user_idx,priority:1"`
	Action     string `gorm:"not null"`
	FromLevel  string `gorm:"not null;default:''"`
	ToLevel    string `gorm:"not null;default:''"`
	Reason     string `gorm:"not null;default:''"`
	OperatorID *int64 `gorm:"index:plg_moderation__watch_events_operator_idx,where:operator_id IS NOT NULL"`
	AnalysisID *int64
	CreatedAt  time.Time `gorm:"not null;default:now();autoCreateTime:false"`
}

func (WatchEvent) TableName() string { return Prefix + "watch_events" }

// Cost is the hourly cost and health of the upstream calls of a channel by
// source, written by adding. Cost is numeric: the text of a decimal.
type Cost struct {
	Hour           time.Time         `gorm:"primaryKey"`
	ChannelID      int64             `gorm:"primaryKey;autoIncrement:false"`
	Source         string            `gorm:"primaryKey"`
	Calls          int64             `gorm:"not null;default:0"`
	PricedCalls    int64             `gorm:"not null;default:0"`
	UnpricedCalls  int64             `gorm:"not null;default:0"`
	UnpricedTokens int64             `gorm:"not null;default:0"`
	FreeCalls      int64             `gorm:"not null;default:0"`
	TokensIn       int64             `gorm:"not null;default:0"`
	TokensOut      int64             `gorm:"not null;default:0"`
	Cost           string            `gorm:"type:numeric(20,8);not null;default:0"`
	Errors         int64             `gorm:"not null;default:0"`
	LatencyMsSum   int64             `gorm:"not null;default:0"`
	Lat            pgdb.Array[int64] `gorm:"type:bigint[];not null;default:'{0,0,0,0,0,0,0,0,0}'"`
}

func (Cost) TableName() string { return Prefix + "costs" }

// StatsHourly is one hour of counters of a (group, flow, stage, auditor,
// timing), written by adding. Lat counts latencies in 9 buckets.
type StatsHourly struct {
	Hour           time.Time         `gorm:"primaryKey"`
	GroupID        int64             `gorm:"primaryKey;autoIncrement:false;not null;default:0"`
	FlowID         int64             `gorm:"primaryKey;autoIncrement:false;not null;default:0"`
	Stage          int               `gorm:"type:integer;primaryKey;autoIncrement:false;not null;default:0"`
	AuditorID      int64             `gorm:"primaryKey;autoIncrement:false;not null;default:0"`
	Timing         string            `gorm:"primaryKey;not null;default:''"`
	Audited        int64             `gorm:"not null;default:0"`
	Hit            int64             `gorm:"not null;default:0"`
	Blocked        int64             `gorm:"not null;default:0"`
	Overturned     int64             `gorm:"not null;default:0"`
	Errors         int64             `gorm:"not null;default:0"`
	CacheSkipped   int64             `gorm:"not null;default:0"`
	Calls          int64             `gorm:"not null;default:0"`
	TokensIn       int64             `gorm:"not null;default:0"`
	TokensOut      int64             `gorm:"not null;default:0"`
	LatencyMsSum   int64             `gorm:"not null;default:0"`
	Lat            pgdb.Array[int64] `gorm:"type:bigint[];not null;default:'{0,0,0,0,0,0,0,0,0}'"`
	Cost           string            `gorm:"type:numeric(20,8);not null;default:0"`
	PricedCalls    int64             `gorm:"not null;default:0"`
	UnpricedCalls  int64             `gorm:"not null;default:0"`
	UnpricedTokens int64             `gorm:"not null;default:0"`
	FreeCalls      int64             `gorm:"not null;default:0"`
	Replayed       int64             `gorm:"not null;default:0"`
}

func (StatsHourly) TableName() string { return Prefix + "stats_hourly" }

// UserDaily counts the moderated requests of a user on a UTC day.
type UserDaily struct {
	Day        time.Time `gorm:"type:date;primaryKey;index:plg_moderation__user_daily_user_idx,priority:2"`
	UserID     int64     `gorm:"primaryKey;autoIncrement:false;index:plg_moderation__user_daily_user_idx,priority:1"`
	GroupID    int64     `gorm:"not null;default:0"`
	Audited    int64     `gorm:"not null;default:0"`
	Suspicious int64     `gorm:"not null;default:0"`
	High       int64     `gorm:"not null;default:0"`
	Errors     int64     `gorm:"not null;default:0"`
	LastHitAt  *time.Time
}

func (UserDaily) TableName() string { return Prefix + "user_daily" }
