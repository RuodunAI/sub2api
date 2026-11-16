package control

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The rows the controller writes, as GORM models. Columns a model leaves out
// keep their database defaults.

// eventRow is one updater.events row; its id and created_at are database
// defaults.
type eventRow struct {
	UpgradeID string
	Kind      string
	Message   string
}

func (eventRow) TableName() string { return "updater.events" }

// insertEvents journals rows in one INSERT.
func insertEvents(db *gorm.DB, rows ...eventRow) error {
	return db.Create(rows).Error
}

// clusterRecord is a new updater.clusters row.
type clusterRecord struct {
	ClusterID   string
	PrimaryNode string
	Baseline    string
}

func (clusterRecord) TableName() string { return "updater.clusters" }

// releaseRecord is an updater.releases row; the documents are JSON.
type releaseRecord struct {
	Digest         string
	ReleaseID      string
	Manifest       []byte
	SignedManifest []byte
	BundleBase     string
}

func (releaseRecord) TableName() string { return "updater.releases" }

// nodeRecord is the updater.nodes row a shell registers.
type nodeRecord struct {
	NodeID       string
	ClusterID    string
	PeerURL      string
	ShellBootID  string
	OS           string `gorm:"column:os"`
	Arch         string
	RuntimeABI   string `gorm:"column:runtime_abi"`
	PeerProtocol int
	Strategy     string
	JoiningPlan  string
	// Rolling is always true: shells 0.1.134 still read it to choose
	// rolling-v1 while the gateways are replaced one at a time.
	Rolling bool
}

func (nodeRecord) TableName() string { return "updater.nodes" }

// stepRecord is a new updater.steps row; its status starts pending.
type stepRecord struct {
	UpgradeID    string
	StepID       int
	NodeID       string
	Action       string
	TargetDigest string
	PeerNode     string
}

func (stepRecord) TableName() string { return "updater.steps" }

// admissionRecord is an updater.node_admissions row.
type admissionRecord struct {
	NodeID            string
	ClusterID         string
	CoreBootID        string
	ReleaseDigest     string
	Revision          int64
	ServeHTTP         bool `gorm:"column:serve_http"`
	ClaimBackground   bool
	CoordinatePlugins bool
}

func (admissionRecord) TableName() string { return "updater.node_admissions" }

// stopConfirmation is an updater.stop_confirmations row; confirmed_at is the
// database's now().
type stopConfirmation struct {
	ClusterID   string
	NodeID      string
	ShellBootID string
	UpgradeID   string
	StepID      *int
	CoreBootID  string
}

func (stopConfirmation) TableName() string { return "updater.stop_confirmations" }

// columns names columns, for an ON CONFLICT target or RETURNING list.
func columns(names ...string) []clause.Column {
	out := make([]clause.Column, len(names))
	for i, n := range names {
		out[i] = clause.Column{Name: n}
	}
	return out
}

// forUpdate and forShare lock the rows a query reads until the transaction
// ends. Each call returns a new clause, so no statement can change another's.
func forUpdate() clause.Locking { return clause.Locking{Strength: clause.LockingStrengthUpdate} }
func forShare() clause.Locking  { return clause.Locking{Strength: clause.LockingStrengthShare} }
