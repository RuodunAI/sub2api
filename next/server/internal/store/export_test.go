package store

// Internals that the external tests (package store_test) reach: the SQL
// snapshots (gormtest imports store, so they cannot live in this package)
// and the hot-path benchmarks.
var (
	ReadQuotaRows       = readQuotaRows
	LockSetting         = lockSetting
	WriteSetting        = writeSetting
	UpsertBalanceSQL    = upsertBalance
	RecordRefreshSQL    = recordAccountRefresh
	SaveAccountQuotaSQL = saveAccountQuota
	ClaimQuotaQuerySQL  = claimAccountQuotaQuery
	SetQuotaErrorSQL    = setAccountQuotaError
	EnsureQuotaSQL      = ensureAccountQuota
	ResetQuotaSQL       = resetAccountQuota
	LeaseQuotaSQL       = leaseAccountQuota
	ReleaseQuotaSQL     = releaseAccountQuota
	CompleteQuotaSQL    = completeAccountQuota
)
