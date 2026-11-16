package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Masterminds/semver/v3"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// BuiltinRequirement is immutable image metadata, read once at startup.
type BuiltinRequirement struct {
	Version     *semver.Version
	InstallOnly bool
}

func (s *Service) BuiltinRequirements(dir string) (map[string]BuiltinRequirement, error) {
	out := map[string]BuiltinRequirement{}
	if dir == "" {
		return out, nil
	}
	only, err := readInstallOnly(dir)
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.s2plugin"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p, err := pkg.Open(data, s.Limits())
		if err != nil {
			return nil, err
		}
		v, err := semver.NewVersion(p.Manifest.Version)
		if err != nil {
			return nil, err
		}
		old, ok := out[p.Manifest.Key]
		if !ok || old.Version.LessThan(v) {
			out[p.Manifest.Key] = BuiltinRequirement{v, only[p.Manifest.Key]}
		}
	}
	return out, nil
}

// BuiltinsReady checks the image's minimum usable version, not instantaneous
// equality with the rollout target. A rolling upgrade continues serving its
// approved old generation until the local atomic switch. Explicitly disabled
// and install-only plugins remain optional.
func (s *Service) BuiltinsReady(ctx context.Context, want map[string]BuiltinRequirement, gen core.Generation) (bool, error) {
	return s.builtinsReady(ctx, want, gen, true)
}

// BuiltinsCommitted reports that the cluster runs the bundle: every builtin
// the bundle carries is approved and, unless disabled or install-only, active
// at least at the bundled version. Unlike BuiltinsReady it does not look at
// this node's instances, which follow on their own (CONTRACTS §36).
func (s *Service) BuiltinsCommitted(ctx context.Context, want map[string]BuiltinRequirement) (bool, error) {
	return s.builtinsReady(ctx, want, nil, false)
}

func (s *Service) builtinsReady(ctx context.Context, want map[string]BuiltinRequirement, gen core.Generation, checkLocal bool) (bool, error) {
	g := store.G(ctx, s.d.DB.Gorm)
	for key, need := range want {
		var local core.PluginInfo
		var hasLocal bool
		if gen != nil {
			local, hasLocal = gen.Plugin(key)
		}
		var status string
		var active *string
		var approved, activeApproved, localApproved bool
		err := builtinState(g, key, need.Version.Original(), local.Version).
			Row().Scan(&status, &active, &approved, &activeApproved, &localApproved)
		if err != nil {
			return false, err
		}
		if !approved {
			return false, nil
		}
		if status == StatusDisabled || (status == StatusInstalled && need.InstallOnly) {
			continue
		}
		if (status != StatusEnabled && status != StatusUpgrading) || active == nil || !activeApproved || (checkLocal && (!hasLocal || !localApproved)) {
			return false, nil
		}
		v, err := semver.NewVersion(*active)
		if err != nil {
			return false, fmt.Errorf("builtin %s active version: %w", key, err)
		}
		if v.LessThan(need.Version) {
			return false, nil
		}
		if !checkLocal {
			continue
		}
		localVersion, err := semver.NewVersion(local.Version)
		if err != nil {
			return false, fmt.Errorf("builtin %s local version: %w", key, err)
		}
		if localVersion.LessThan(need.Version) {
			return false, nil
		}
	}
	return true, nil
}

// builtinState selects the status and active version of built-in plugin key
// and whether its versions need, active and local are approved.
func builtinState(db *gorm.DB, key, need, local string) *gorm.DB {
	approved := func(version any) *gorm.DB {
		return db.Table("plugin_versions AS v").Select("1").
			Where("v.plugin_key = plugins.key AND v.version = ? AND v.consent_status = 'approved'", version)
	}
	return db.Model(&model.Plugin{}).
		Select("status, active_version, EXISTS (?), EXISTS (?), EXISTS (?)",
			approved(need), approved(gorm.Expr("plugins.active_version")), approved(local)).
		Where("key = ? AND builtin", key)
}
