package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Masterminds/semver/v3"
)

func TestPackRefusesToOverwriteExistingIdentity(t *testing.T) {
	dir, market := demoPlugin(t), t.TempDir()
	pkg := strings.TrimSpace(runOK(t, "pack", "--dir", dir, "--out-dir", market))
	original, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	// The overlay adds a migration but accidentally keeps the release version.
	writeFile(t, filepath.Join(dir, "testdata", "v2", "manifest.patch.json"), `{}`)
	for _, output := range [][]string{{"--out-dir", market}, {"--out", pkg}} {
		args := append([]string{"pack", "--dir", dir, "--overlay", "testdata/v2"}, output...)
		if msg := runFail(t, args...); !strings.Contains(msg, "refusing to overwrite existing package") {
			t.Fatalf("collision error = %s", msg)
		}
		got, err := os.ReadFile(pkg)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf("colliding fixture changed the release: %v", err)
		}
	}
}

func TestConcurrentPackCannotReplaceWinner(t *testing.T) {
	dir, market := demoPlugin(t), t.TempDir()
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			var out, errOut bytes.Buffer
			codes <- run([]string{"pack", "--dir", dir, "--out-dir", market}, &out, &errOut)
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	succeeded := 0
	for code := range codes {
		if code == 0 {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent packs succeeded, want exactly one", succeeded)
	}
	if _, err := readPackage(filepath.Join(market, "demo-0.1.0.s2plugin")); err != nil {
		t.Fatalf("winner is not a complete package: %v", err)
	}
}

// Use the real manifests and overlays. The image's built-in selector takes
// the package matching the release manifest version, which must never be a
// fixture: every plugin's upgrade fixture (testdata/upgrade) packs under its
// own, higher version, with its own migrations, and leaves the release as it
// was.
func TestReleaseAndUpgradeFixturesRemainDistinct(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "plugins", "*", "testdata", "upgrade"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no upgrade fixtures: %v", err)
	}
	for _, overlay := range fixtures {
		dir := filepath.Dir(filepath.Dir(overlay))
		t.Run(filepath.Base(dir), func(t *testing.T) {
			market, runtimes := t.TempDir(), t.TempDir()
			for _, platform := range []string{"linux-amd64", "linux-arm64"} {
				writeFile(t, filepath.Join(runtimes, "runtimes", platform, "plugin"), "fake runtime for package contents")
			}
			// The package names only: the native UI is built separately.
			release := strings.TrimSpace(runOK(t, "pack", "--dir", dir, "--runtimes", runtimes, "--out-dir", market, "--allow-missing-ui"))
			before, err := os.ReadFile(release)
			if err != nil {
				t.Fatal(err)
			}
			// A fixture's migrations have no model on purpose: db check,
			// which includes the overlay's migrations, does not apply to it.
			fixture := strings.TrimSpace(runOK(t, "pack", "--dir", dir, "--runtimes", runtimes,
				"--overlay", "testdata/upgrade", "--skip-db-check", "--out-dir", market, "--allow-missing-ui"))
			shipped, patched := jsonVersion(t, filepath.Join(dir, "manifest.json")), jsonVersion(t, filepath.Join(overlay, "manifest.patch.json"))
			if !semver.MustParse(patched).GreaterThan(semver.MustParse(shipped)) {
				t.Fatalf("fixture version %s is not above the release %s", patched, shipped)
			}
			key := filepath.Base(dir)
			if filepath.Base(release) != key+"-"+shipped+".s2plugin" || filepath.Base(fixture) != key+"-"+patched+".s2plugin" {
				t.Fatalf("release=%s fixture=%s", release, fixture)
			}
			after, _ := os.ReadFile(release)
			if !bytes.Equal(before, after) {
				t.Fatal("packaging the upgrade fixture changed the release")
			}
			migrations, _ := filepath.Glob(filepath.Join(overlay, "migrations", "*.sql"))
			for _, tc := range []struct {
				path    string
				version string
				fixture bool
			}{{release, shipped, false}, {fixture, patched, true}} {
				files, err := readPackage(tc.path)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range migrations {
					if _, ok := files["migrations/"+filepath.Base(m)]; ok != tc.fixture {
						t.Fatalf("%s: fixture migration %s presence=%v", tc.path, filepath.Base(m), ok)
					}
				}
				if v := versionOf(files["manifest.json"]); v != tc.version {
					t.Fatalf("%s: packaged manifest version %q, want %s", tc.path, v, tc.version)
				}
			}
		})
	}
}

// jsonVersion reads the "version" of a manifest or manifest patch file.
func jsonVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v := versionOf(raw)
	if v == "" {
		t.Fatalf("%s has no version", path)
	}
	return v
}

func versionOf(raw []byte) string {
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Version
}
