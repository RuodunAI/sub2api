package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/updatesource"
)

// mirror copies stable releases of an update source (a GitHub repository or
// another url source) into a directory that serves as a url update source:
// <line>/<version>/<asset> plus releases.json. Listing and downloads use the
// gateway's own update sources; assets are copied byte for byte, so every
// manifest keeps its digest.

const (
	releasesIndex = "releases.json"
	maxAsset      = 16 << 30
)

func mirror(args []string, out io.Writer) error {
	f := flag.NewFlagSet("mirror", flag.ContinueOnError)
	dest := f.String("out", "", "mirror directory (<line>/<version>/<asset> and "+releasesIndex+")")
	repo := f.String("github", "", "source GitHub repository owner/repo")
	base := f.String("url", "", "source url update source: the https directory of its "+releasesIndex)
	secretEnv := f.String("token-env", "", "environment variable holding the GitHub token or the url source's Bearer secret")
	tags := f.String("tags", "", "comma separated tags, e.g. core-v0.1.291,gateway-v0.1.136")
	latest := f.Int("latest", 0, fmt.Sprintf("mirror the newest N stable releases of each line (GitHub: among its %d newest releases)", updatesource.MaxGitHubReleases))
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dest == "" || (*repo == "") == (*base == "") || (*tags == "") == (*latest <= 0) {
		return errors.New("--out, one of --github or --url, and one of --tags or --latest are required")
	}
	cfg, err := mirrorConfig(*repo, *base, *secretEnv)
	if err != nil {
		return err
	}
	// An operator tool: an intranet mirror is a legitimate source.
	src, err := updatesource.New(cfg, updatesource.Options{AllowPrivate: true})
	if err != nil {
		return err
	}
	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}
	n, err := mirrorReleases(context.Background(), src, *dest, tagList, *latest)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "mirrored %d release(s) into %s\n", n, *dest)
	return err
}

func mirrorConfig(repo, base, secretEnv string) (updatesource.Config, error) {
	cfg := updatesource.Config{Enabled: true, Kind: updatesource.GitHub}
	var err error
	if repo != "" {
		cfg.Repository, err = updatesource.NormalizeRepository(repo)
	} else {
		cfg.Kind = updatesource.URL
		cfg.URL, err = updatesource.NormalizeURL(base)
	}
	if err != nil {
		return cfg, err
	}
	if secretEnv != "" {
		if cfg.Secret = os.Getenv(secretEnv); cfg.Secret == "" {
			return cfg, fmt.Errorf("environment variable %s is empty", secretEnv)
		}
	}
	return cfg, nil
}

func mirrorReleases(ctx context.Context, src updatesource.Source, dest string, tags []string, latest int) (int, error) {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return 0, err
	}
	all, err := src.Releases(ctx)
	if err != nil {
		return 0, err
	}
	selected, err := selectReleases(all, tags, latest)
	if err != nil {
		return 0, err
	}
	entries := make([]updatesource.IndexRelease, 0, len(selected))
	for _, l := range selected {
		entry, err := download(ctx, src, l, dest)
		if err != nil {
			return 0, err
		}
		entries = append(entries, entry)
	}
	return len(entries), mergeIndex(filepath.Join(dest, releasesIndex), entries)
}

func tagOf(line, version string) string { return line + "-v" + version }

// selectReleases picks the named tags, or the newest latest releases per line.
// Sources list newest first within each line.
func selectReleases(all []updatesource.Listing, tags []string, latest int) ([]updatesource.Listing, error) {
	if latest > 0 {
		count := map[string]int{}
		return slices.DeleteFunc(slices.Clone(all), func(l updatesource.Listing) bool {
			count[l.Line]++
			return count[l.Line] > latest
		}), nil
	}
	out := make([]updatesource.Listing, 0, len(tags))
	for _, tag := range tags {
		i := slices.IndexFunc(all, func(l updatesource.Listing) bool { return tagOf(l.Line, l.Version) == tag })
		if i < 0 {
			return nil, fmt.Errorf("stable release %s not found at the source", tag)
		}
		out = append(out, all[i])
	}
	return out, nil
}

func download(ctx context.Context, src updatesource.Source, l updatesource.Listing, dest string) (updatesource.IndexRelease, error) {
	entry := updatesource.IndexRelease{Line: l.Line, Version: l.Version, PublishedAt: l.PublishedAt, Notes: l.Notes, Page: l.Page, Assets: map[string]updatesource.IndexAsset{}}
	dir := filepath.Join(dest, l.Line, l.Version)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return entry, err
	}
	for name, a := range l.Assets {
		rel := l.Line + "/" + l.Version + "/" + name
		if a.Size > maxAsset {
			return entry, fmt.Errorf("%s: asset too large", rel)
		}
		dst := filepath.Join(dir, name)
		// Releases are immutable: a complete earlier copy is kept.
		if info, err := os.Stat(dst); err != nil || info.Size() != a.Size {
			if err = fetch(ctx, src, a, dst); err != nil {
				return entry, fmt.Errorf("%s: %w", rel, err)
			}
		}
		entry.Assets[name] = updatesource.IndexAsset{Path: rel, Size: a.Size}
	}
	return entry, nil
}

func fetch(ctx context.Context, src updatesource.Source, a updatesource.Asset, dst string) error {
	body, err := src.Open(ctx, a, a.Size)
	if err != nil {
		return err
	}
	defer body.Close()
	return publish(dst, func(w io.Writer) error {
		n, err := io.Copy(w, body)
		if err == nil && n != a.Size {
			err = fmt.Errorf("downloaded %d bytes, the source lists %d", n, a.Size)
		}
		return err
	})
}

// publish writes path through a temporary file in its directory, made
// world-readable like the rest of the served directory, and renames it into
// place, so that a reader never sees a partial file.
func publish(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mirror-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = write(tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0644)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// compareReleases orders by line, then newest version first.
func compareReleases(a, b updatesource.IndexRelease) int {
	if a.Line != b.Line {
		return strings.Compare(a.Line, b.Line)
	}
	va, errA := semver.NewVersion(a.Version)
	vb, errB := semver.NewVersion(b.Version)
	if errA != nil || errB != nil {
		return strings.Compare(b.Version, a.Version)
	}
	return vb.Compare(va)
}

// mergeIndex adds entries to the index at path, replacing the same line and
// version, and writes it atomically.
func mergeIndex(path string, entries []updatesource.IndexRelease) error {
	idx := updatesource.Index{Schema: updatesource.IndexSchema}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err = json.Unmarshal(data, &idx); err != nil || idx.Schema != updatesource.IndexSchema {
			return fmt.Errorf("%s is not a %s index", path, updatesource.IndexSchema)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	for _, e := range entries {
		idx.Releases = slices.DeleteFunc(idx.Releases, func(o updatesource.IndexRelease) bool { return o.Line == e.Line && o.Version == e.Version })
		idx.Releases = append(idx.Releases, e)
	}
	slices.SortStableFunc(idx.Releases, compareReleases)
	if data, err = json.MarshalIndent(idx, "", "  "); err != nil {
		return err
	}
	return publish(path, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	})
}
