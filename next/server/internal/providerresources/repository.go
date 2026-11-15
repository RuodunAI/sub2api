package providerresources

import (
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const columns = `public_id,plugin_key,kind,remote_id,state,operation_id,user_id,group_id,account_id,principal_id,generation,bytes,metadata,created_at,expires_at,intent_hash`

// resources selects the columns of provider_resources a ProviderResource
// and its intent hash hold.
func resources(db *gorm.DB) *gorm.DB {
	return db.Model(&model.ProviderResource{}).Select(columns)
}

// resource is the ProviderResource a row holds; a NULL expiry is the zero
// time.
func resource(row model.ProviderResource) core.ProviderResource {
	r := core.ProviderResource{
		PublicID: row.PublicID, PluginKey: row.PluginKey, Kind: row.Kind, RemoteID: row.RemoteID, State: row.State, OperationID: row.OperationID,
		Owner:   core.ResourceOwner{UserID: row.UserID, GroupID: row.GroupID},
		Binding: core.ResourceBinding{AccountID: row.AccountID, PrincipalID: row.PrincipalID, Generation: row.Generation},
		Bytes:   row.Bytes, Metadata: row.Metadata, CreatedAt: row.CreatedAt,
	}
	if row.ExpiresAt != nil {
		r.ExpiresAt = *row.ExpiresAt
	}
	return r
}

// The tables whose rows update moves.
const (
	resourcesTable = "provider_resources"
	versionsTable  = "provider_skill_versions"
)

// update writes set to the row of public ID id in table (resourcesTable or
// versionsTable) and moves its updated_at to now(), as every state change
// of these tables does.
func update(db *gorm.DB, table, id string, set map[string]any) *gorm.DB {
	set["updated_at"] = gorm.Expr("now()")
	return db.Table(table).Where("public_id = ?", id).Updates(set)
}

// read reads the one resource that cond (with args) selects, and its intent
// hash.
func read(db *gorm.DB, cond string, args ...any) (core.ProviderResource, string, error) {
	var row model.ProviderResource
	err := resources(db).Where(cond, args...).Take(&row).Error
	return resource(row), row.IntentHash, err
}
