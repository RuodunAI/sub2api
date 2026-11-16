package store

// Internals that the external tests (package store_test) reach: the SQL
// snapshots (gormtest imports store, so they cannot live in this package)
// and the hot-path benchmarks.
var (
	NewQuotaRead        = newQuotaRead
	AddQuotaRow         = (*QuotaRead).add
	QuotasAtSQL         = quotasAt
	SettingValueSQL     = settingValue
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
	ApplyObservationSQL = applyQuotaObservation
	XactLockSQL         = xactLock
)
