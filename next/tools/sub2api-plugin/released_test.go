package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// releasedFile records, per plugin directory, the version production runs
// and the digest (pluginDigest) of the sources it was built from.
var releasedFile = filepath.Join("..", "..", "plugins", "released.json")

// recordReleased rewrites releasedFile from the plugins in the directory it
// names (after a release: the plugins/ of the released commit):
//
//	go test -run TestReleasedPluginsBumped -args -record-released=../../plugins
var recordReleased = flag.String("record-released", "", "plugins directory to record in "+releasedFile)

type releasedPlugin struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type released struct {
	Comment string                    `json:"_comment"`
	Plugins map[string]releasedPlugin `json:"plugins"`
}

// A built-in plugin whose package content changed must get a new version:
// the core keeps the package it already has under a version (an upload of
// the same version with other content only logs a warning), so the change
// would never reach a deployment (review 1116 R2, T2).
func TestReleasedPluginsBumped(t *testing.T) {
	if *recordReleased != "" {
		recordReleasedPlugins(t, *recordReleased)
		return
	}
	b, err := os.ReadFile(releasedFile)
	if err != nil {
		t.Fatal(err)
	}
	var rec released
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	current := pluginVersions(t, filepath.Dir(releasedFile))
	for name, version := range current {
		r, ok := rec.Plugins[name]
		if !ok || r.Version != version {
			continue // new, or bumped since the release
		}
		digest, err := pluginDigest(filepath.Join(filepath.Dir(releasedFile), name))
		if err != nil {
			t.Fatal(err)
		}
		if digest != r.Digest {
			t.Errorf("plugin %s: what its package is built from changed since %s was released (%s), but the version did not: bump the manifest version (and the plugin's Version constant, if it has one)",
				name, version, releasedFile)
		}
	}
}

// pluginVersions maps every plugin directory below root to its manifest
// version.
func pluginVersions(t *testing.T, root string) map[string]string {
	t.Helper()
	manifests, err := filepath.Glob(filepath.Join(root, "*", "manifest.json"))
	if err != nil || len(manifests) == 0 {
		t.Fatalf("no plugins in %s: %v", root, err)
	}
	out := map[string]string{}
	for _, m := range manifests {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Version string }
		if err := json.Unmarshal(b, &v); err != nil || v.Version == "" {
			t.Fatalf("%s: no version (%v)", m, err)
		}
		out[filepath.Base(filepath.Dir(m))] = v.Version
	}
	return out
}

func recordReleasedPlugins(t *testing.T, root string) {
	rec := released{
		Comment: "The plugin versions production runs and the digests of their sources: TestReleasedPluginsBumped (tools/sub2api-plugin) fails when a plugin's sources changed and its version did not. Rewrite after each release.",
		Plugins: map[string]releasedPlugin{},
	}
	for name, version := range pluginVersions(t, root) {
		digest, err := pluginDigest(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		rec.Plugins[name] = releasedPlugin{Version: version, Digest: digest}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasedFile, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

var (
	// unpackedDirs hold no package input: tests, dependencies, build
	// outputs.
	unpackedDirs = map[string]bool{"testdata": true, "node_modules": true, "dist": true, "runtimes": true, "__tests__": true}
	// packedDirs are copied into the package (pack) or built into its ui.
	packedDirs = append([]string{"ui/"}, func() (out []string) {
		for _, d := range packDirs {
			out = append(out, d+"/")
		}
		return out
	}()...)
	// unpackedFiles are tests, docs and lock files: dependency-only changes
	// (go.mod as well; the SDK lives outside the plugin too) rebuild the
	// same plugin, and a release may keep the package production has.
	unpackedFiles = regexp.MustCompile(`(_test\.go|\.(test|spec)\.[cm]?[jt]sx?|\.md)$|(^|/)(go\.mod|go\.sum|package-lock\.json)$`)
)

// pluginDigest hashes what the package of the plugin in dir is built from:
// manifest.json, the directories pack copies, ui/ and every Go file of the
// plugin's binaries; see unpackedDirs and unpackedFiles for what is left out.
func pluginDigest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (unpackedDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		packed := rel == "manifest.json" || strings.HasSuffix(rel, ".go")
		for _, prefix := range packedDirs {
			packed = packed || strings.HasPrefix(rel, prefix)
		}
		if !packed || unpackedFiles.MatchString(rel) || strings.HasPrefix(d.Name(), ".") || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		h.Write(b)
		return nil
	})
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), err
}
