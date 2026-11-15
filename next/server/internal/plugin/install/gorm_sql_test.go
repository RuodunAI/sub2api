package install

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/grants"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the install statements built with GORM (see package gormtest).

// --- plugins: every UPDATE writes updated_at exactly where the raw SQL did.

func TestSQLMarkBuiltin(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markBuiltin(db, "demo") })
}

func TestSQLMarkInstalled(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markInstalled(db, "demo", []byte(`{"en":"Demo"}`), 7) })
}

// The system installs with no actor: installed_by keeps what it holds.
func TestSQLMarkInstalledBySystem(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markInstalled(db, "demo", []byte(`{"en":"Demo"}`), 0) })
}

func TestSQLBumpPlugin(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return bumpPlugin(db, "demo") })
}

func TestSQLBumpPluginReturningTrust(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row struct {
			Status string
		}
		return bumpPluginReturningTrust(db, "demo").Scan(&row)
	})
}

func TestSQLSetPublisherPluginsReason(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return setPublisherPluginsReason(db, 3, "publisher revoked: leaked key") })
}

func TestSQLDisableForUninstall(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return disableForUninstall(db, "demo") })
}

// --- other tables with updated_at.

func TestSQLMarkNodesStopped(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markNodesStopped(db, "demo", store.Strings{"boot-a", "boot-b"}) })
}

func TestSQLMarkCleanupsCleaned(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return markCleanupsCleaned(db, "demo", store.Strings{"boot-a"}) })
}

// --- locks.

func TestSQLLoadConsentablePlugin(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var p consentablePlugin
		return lockedPlugin(db, "demo").Select(consentablePluginColumns).Take(&p)
	})
}

func TestSQLLoadConsentableVersion(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var v model.PluginVersion
		return versionRow(db, "demo", "1.2.0").Select("consent_status, signature_status, manifest_hash").Clauses(forUpdate).Take(&v)
	})
}

func TestSQLUninstallState(t *testing.T) {
	for name, lock := range map[string]bool{"read": false, "locked": true} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
				var row model.PluginUninstall
				return uninstallState(db, "demo", lock).Take(&row)
			})
		})
	}
}

func TestSQLUninstallStopped(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var stopped bool
		return uninstallRow(db, "demo").Select("target_boot_ids <@ stopped_boot_ids").Clauses(forUpdate).Take(&stopped)
	})
}

// --- uninstall barrier.

func TestSQLBeginUninstall(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return beginUninstall(db, "demo", 5, []string{"boot-a"}) })
}

// No live node: the targets are the nodes still running the plugin, and
// the parameter is the empty array, not NULL.
func TestSQLBeginUninstallNoLiveNode(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return beginUninstall(db, "demo", 5, nil) })
}

func TestSQLAddUninstallTargets(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var stopped bool
		return addUninstallTargets(db, "demo", []string{"boot-a", "boot-b"}).Scan(&stopped)
	})
}

func TestSQLAddStoppedBoots(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return addStoppedBoots(db, "demo", store.Strings{"boot-a"}) })
}

func TestSQLRolloutOpen(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var open bool
		return selectExists(db, db.Model(&model.PluginRollout{}).Where("plugin_key = ? AND phase IN ('preparing','activating')", "demo")).Scan(&open)
	})
}

func TestSQLRetireKey(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return retire(db, "demo", 7) })
}

func TestSQLRetiredKeys(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []retiredKeyRow
		return retiredKeys(db).Scan(&rows)
	})
}

// --- inserts: columns with a default are left out where the raw SQL left
// them out, and consent_status is written.

func TestSQLInsertPlugin(t *testing.T) {
	m := &manifest.Manifest{Key: "demo", Name: map[string]string{"en": "Demo"}}
	pub := int64(3)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Create(newPlugin(m, &pub, 7)) })
}

func TestSQLInsertVersion(t *testing.T) {
	p := &pkg.Package{Manifest: &manifest.Manifest{Key: "demo", Version: "1.2.0"}, ManifestRaw: []byte(`{"key":"demo"}`),
		ManifestHash: "mh", SHA256: "sha", Raw: make([]byte, 42)}
	ver := &pkg.Verification{SignatureStatus: pkg.SigUnsigned}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Create(newVersion(p, ver, UploadOptions{}, 0)) })
}

func TestSQLInsertPublisherKey(t *testing.T) {
	notAfter := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Create(newPublisherKey(3, KeyInput{KeyID: "k1", PublicKey: "pk", NotAfter: &notAfter}))
	})
}

// --- reads.

func TestSQLPruneGrants(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return pruneGrants(db, "demo", []string{"http.outbound", "kv"}) })
}

// A manifest requesting nothing prunes every grant but revocations.
func TestSQLPruneGrantsNoneRequested(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return pruneGrants(db, "demo", []string{}) })
}

func TestSQLPluginGrants(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []grantRow
		return pluginGrants(db, "demo").Scan(&rows)
	})
}

// grants.VersionRows takes key and version several times each, as $1 and
// $2: a change of its placeholders must change versionRowsNamed too.
func TestSQLVersionGrants(t *testing.T) {
	if strings.Contains(versionRowsNamed, "$") || !strings.Contains(versionRowsNamed, "@key") || !strings.Contains(versionRowsNamed, "@version") {
		t.Fatalf("grants.VersionRows no longer takes $1 and $2:\n%s", grants.VersionRows)
	}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []grantRow
		return versionGrants(db, "demo", "1.2.0").Scan(&rows)
	})
}

func TestSQLPendingRoleGrants(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []pendingRoleGrantRow
		return pendingRoleGrants(db, "demo", "1.2.0").Scan(&rows)
	})
}

func TestSQLLatestConsentDecisions(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row struct{ Raw json.RawMessage }
		return latestConsentDecisions(db, "demo", "1.2.0").Scan(&row)
	})
}

func TestSQLRoleColumns(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []roleColumnRow
		return roleColumns(db).Find(&rows)
	})
}

func TestSQLApprovedResources(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []resourceRow
		return approvedResources(db, "demo").Find(&rows)
	})
}

func TestSQLBuiltinState(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var row struct{ Status string }
		return builtinState(db, "demo", "1.2.0", "1.1.0").Scan(&row)
	})
}

func TestSQLVersions(t *testing.T) {
	var v string
	t.Run("current", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return currentVersion(db, "demo").Scan(&v) })
	})
	t.Run("running", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return runningVersion(db, "demo", "1.2.0").Scan(&v) })
	})
	t.Run("reviewed", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var row struct{ Status string }
			return reviewedVersion(db, "demo", "1.2.0").Scan(&row)
		})
	})
}

func TestSQLRunningSignedBy(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var keys []string
		return runningSignedBy(db, "k1").Scan(&keys)
	})
}
