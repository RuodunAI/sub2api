package install

import (
	"context"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// PluginDatabase is what GET /plugins/:key/database shows of a plugin's
// database side (CONTRACTS §130), for the version the plugin runs (or the
// one it would, CurrentVersion).
type PluginDatabase struct {
	// Prefix every table, index and sequence of the plugin carries.
	Prefix string `json:"prefix"`
	// Role is the plugin's database identity.
	Role string `json:"role"`
	// Access is the version's database.access.
	Access []manifest.DatabaseAccess `json:"access"`
	// Migrations are the version's scripts with when each was applied.
	Migrations []DatabaseMigration `json:"migrations"`
}

// DatabaseMigration is a migration script of a plugin.
type DatabaseMigration struct {
	ID        string     `json:"id"`
	Checksum  string     `json:"checksum"`
	AppliedAt *time.Time `json:"applied_at"` // nil: not applied
	SQL       string     `json:"sql"`
}

// Database describes the database side of plugin key.
func (s *Service) Database(ctx context.Context, key string) (*PluginDatabase, error) {
	if s.d.Schemas == nil {
		return nil, core.ErrUnavailable.WithMessage("plugin databases are unavailable on this node")
	}
	version, err := CurrentVersion(ctx, s.d.DB.Gorm, key)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, core.ErrNotFound.WithMessage("plugin has no versions")
	}
	p, err := s.Package(ctx, key, version)
	if err != nil {
		return nil, err
	}
	out := &PluginDatabase{Prefix: manifest.TablePrefix(key), Role: s.d.Schemas.Identity(key),
		Access: []manifest.DatabaseAccess{}, Migrations: []DatabaseMigration{}}
	if p.Manifest.Database == nil {
		return out, nil
	}
	out.Access = append(out.Access, p.Manifest.Database.Access...)
	applied, err := s.d.Schemas.AppliedMigrations(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, mg := range migrationScripts(p.Manifest, p.Files) {
		dm := DatabaseMigration{ID: mg.ID, Checksum: mg.Checksum, SQL: mg.SQL}
		if rec, ok := applied[mg.ID]; ok {
			dm.AppliedAt = &rec.AppliedAt
		}
		out.Migrations = append(out.Migrations, dm)
	}
	return out, nil
}
