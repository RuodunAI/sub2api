package store

import "gorm.io/gorm"

// AuthzVersion reads the authz version (authz_meta, bumped by every authz
// write); read in a snapshot, it is the version the snapshot shows. The
// legacy loader and Casbin's both label what they read with it.
func AuthzVersion(db *gorm.DB) (int64, error) {
	var v int64
	err := db.Table("authz_meta").Select("version").Where("id = 1").Row().Scan(&v)
	return v, err
}
