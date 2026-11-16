package store

import "gorm.io/gorm"

// AuthzVersions reads the authz version (authz_meta.version, moved by every
// authz write) and the policy version (authz_meta.policy_version, moved only
// by the writes of grants, permission statuses and role definitions) in one
// statement; read in a snapshot, they are the versions the snapshot shows.
func AuthzVersions(db *gorm.DB) (version, policy int64, err error) {
	err = db.Raw(`SELECT version, policy_version FROM authz_meta WHERE id = 1`).Row().Scan(&version, &policy)
	return version, policy, err
}
