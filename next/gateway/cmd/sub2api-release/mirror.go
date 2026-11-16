package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// mirror copies stable releases of a GitHub repository, or of another url
// update source, into a directory that serves as a url update source:
// <line>/<version>/<asset> plus releases.json (DESIGN-2026-11-17 §4). Assets
// are copied byte for byte, so manifests keep their original signatures.

const (
	releasesIndex  = "releases.json"
	releasesSchema = "sub2api-releases-v1"
	githubAPI      = "https://api.github.com"
	maxListing     = 2 << 20
	maxAsset       = 16 << 30
)

var (
	releaseTag  = regexp.MustCompile(`^(core|gateway)-v(.+)$`)
	githubRepo  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	pathSegment = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*$`)
)

type releaseIndex struct {
	Schema   string         `json:"schema"`
	Releases []indexRelease `json:"releases"`
}

type indexRelease struct {
	Line        string                `json:"line"`
	Version     string                `json:"version"`
	PublishedAt time.Time             `json:"published_at"`
	Notes       string                `json:"notes"`
	Page        string                `json:"page"`
	Assets      map[string]indexAsset `json:"assets"`
}

type indexAsset struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// sourceRelease is a listed release with the download URL of each asset.
type sourceRelease struct {
	indexRelease
	urls map[string]string
}

type mirrorSource struct {
	client *http.Client
	api    string // GitHub API base: the only origin a github token goes to
	repo   string // github source: owner/repo
	base   string // url source: https base ending in "/"
	secret string
}

func mirror(args []string, out io.Writer) error {
	f := flag.NewFlagSet("mirror", flag.ContinueOnError)
	dest := f.String("out", "", "mirror directory (<line>/<version>/<asset> and "+releasesIndex+")")
	repo := f.String("github", "", "source GitHub repository owner/repo")
	base := f.String("url", "", "source url update source: the https directory of its "+releasesIndex)
	secretEnv := f.String("token-env", "", "environment variable holding the GitHub token or the url source's Bearer secret")
	tags := f.String("tags", "", "comma separated tags, e.g. core-v0.1.291,gateway-v0.1.136")
	latest := f.Int("latest", 0, "mirror the newest N stable releases of each line")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dest == "" || (*repo == "") == (*base == "") || (*tags == "") == (*latest <= 0) {
		return errors.New("--out, one of --github or --url, and one of --tags or --latest are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Minute
	src := mirrorSource{client: &http.Client{Transport: transport}, api: githubAPI, repo: *repo}
	if *secretEnv != "" {
		if src.secret = os.Getenv(*secretEnv); src.secret == "" {
			return fmt.Errorf("environment variable %s is empty", *secretEnv)
		}
	}
	if *repo != "" && !githubRepo.MatchString(*repo) {
		return errors.New("--github must be owner/repo")
	}
	if *base != "" {
		var err error
		if src.base, err = normalizeBase(*base); err != nil {
			return err
		}
	}
	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}
	n, err := src.mirror(context.Background(), *dest, tagList, *latest)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "mirrored %d release(s) into %s\n", n, *dest)
	return err
}

func (s mirrorSource) mirror(ctx context.Context, dest string, tags []string, latest int) (int, error) {
	client := *s.client
	client.CheckRedirect = stripCrossOriginAuth
	s.client = &client
	if err := os.MkdirAll(dest, 0755); err != nil {
		return 0, err
	}
	all, err := s.releases(ctx)
	if err != nil {
		return 0, err
	}
	selected, err := selectReleases(all, tags, latest)
	if err != nil {
		return 0, err
	}
	entries := make([]indexRelease, 0, len(selected))
	for _, r := range selected {
		entry, err := s.download(ctx, r, dest)
		if err != nil {
			return 0, err
		}
		entries = append(entries, entry)
	}
	return len(entries), mergeIndex(filepath.Join(dest, releasesIndex), entries)
}

func (s mirrorSource) releases(ctx context.Context) ([]sourceRelease, error) {
	if s.repo != "" {
		return s.githubReleases(ctx)
	}
	return s.indexReleases(ctx)
}

type githubRelease struct {
	Tag         string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Body        string    `json:"body"`
	Page        string    `json:"html_url"`
	Assets      []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (s mirrorSource) githubReleases(ctx context.Context) ([]sourceRelease, error) {
	var out []sourceRelease
	for page := 1; page <= 10; page++ {
		var batch []githubRelease
		if err := s.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", s.api, s.repo, page), "application/vnd.github+json", &batch); err != nil {
			return nil, err
		}
		for _, g := range batch {
			line, version, ok := parseReleaseTag(g.Tag)
			if !ok || g.Draft || g.Prerelease {
				continue
			}
			r := sourceRelease{indexRelease{Line: line, Version: version, PublishedAt: g.PublishedAt, Notes: g.Body, Page: g.Page, Assets: map[string]indexAsset{}}, map[string]string{}}
			for _, a := range g.Assets {
				r.Assets[a.Name] = indexAsset{Size: a.Size}
				r.urls[a.Name] = fmt.Sprintf("%s/repos/%s/releases/assets/%d", s.api, s.repo, a.ID)
			}
			out = append(out, r)
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

func (s mirrorSource) indexReleases(ctx context.Context) ([]sourceRelease, error) {
	var idx releaseIndex
	if err := s.getJSON(ctx, s.base+releasesIndex, "application/json", &idx); err != nil {
		return nil, err
	}
	if idx.Schema != releasesSchema {
		return nil, fmt.Errorf("source index schema %q is not %s", idx.Schema, releasesSchema)
	}
	out := make([]sourceRelease, 0, len(idx.Releases))
	for _, r := range idx.Releases {
		if _, _, ok := parseReleaseTag(r.Line + "-v" + r.Version); !ok {
			return nil, fmt.Errorf("source index lists an invalid release %s %s", r.Line, r.Version)
		}
		sr := sourceRelease{r, map[string]string{}}
		for name, a := range r.Assets {
			if !validPath(a.Path) {
				return nil, fmt.Errorf("source index: invalid asset path %q", a.Path)
			}
			sr.urls[name] = s.base + a.Path
		}
		out = append(out, sr)
	}
	return out, nil
}

// selectReleases picks the named tags, or the newest latest releases per line.
func selectReleases(all []sourceRelease, tags []string, latest int) ([]sourceRelease, error) {
	slices.SortStableFunc(all, func(a, b sourceRelease) int { return compareReleases(a.indexRelease, b.indexRelease) })
	var out []sourceRelease
	if latest > 0 {
		count := map[string]int{}
		for _, r := range all {
			if count[r.Line] < latest {
				count[r.Line]++
				out = append(out, r)
			}
		}
		return out, nil
	}
	for _, tag := range tags {
		i := slices.IndexFunc(all, func(r sourceRelease) bool { return r.Line+"-v"+r.Version == tag })
		if i < 0 {
			return nil, fmt.Errorf("stable release %s not found at the source", tag)
		}
		out = append(out, all[i])
	}
	return out, nil
}

func (s mirrorSource) download(ctx context.Context, r sourceRelease, dest string) (indexRelease, error) {
	entry := r.indexRelease
	entry.Assets = map[string]indexAsset{}
	dir := filepath.Join(dest, r.Line, r.Version)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return entry, err
	}
	for name, a := range r.Assets {
		rel := r.Line + "/" + r.Version + "/" + name
		if !pathSegment.MatchString(name) || a.Size <= 0 || a.Size > maxAsset {
			return entry, fmt.Errorf("%s: invalid asset name or size", rel)
		}
		dst := filepath.Join(dir, name)
		// Releases are immutable: a complete earlier copy is kept.
		if info, err := os.Stat(dst); err != nil || info.Size() != a.Size {
			if err = s.fetch(ctx, r.urls[name], dst, a.Size); err != nil {
				return entry, fmt.Errorf("%s: %w", rel, err)
			}
		}
		entry.Assets[name] = indexAsset{Path: rel, Size: a.Size}
	}
	return entry, nil
}

func (s mirrorSource) fetch(ctx context.Context, rawURL, dst string, size int64) error {
	res, err := s.get(ctx, rawURL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mirror-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(res.Body, size+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("downloaded %d bytes, the source lists %d", n, size)
	}
	return os.Rename(tmp.Name(), dst)
}

func (s mirrorSource) getJSON(ctx context.Context, rawURL, accept string, v any) error {
	res, err := s.get(ctx, rawURL, accept)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxListing+1))
	if err != nil {
		return err
	}
	if len(data) > maxListing {
		return errors.New("release listing too large")
	}
	return json.Unmarshal(data, v)
}

// get sends the secret only to the source's own origin (the GitHub API or the
// url source); redirects to other origins drop it (stripCrossOriginAuth).
func (s mirrorSource) get(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	origin := s.base
	if s.repo != "" {
		origin = s.api
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	if s.secret != "" && sameOrigin(req.URL, origin) {
		req.Header.Set("Authorization", "Bearer "+s.secret)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", req.URL.Redacted(), res.Status)
	}
	return res, nil
}

func stripCrossOriginAuth(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if !sameOrigin(req.URL, via[0].URL.String()) {
		req.Header.Del("Authorization")
	}
	return nil
}

func sameOrigin(u *url.URL, origin string) bool {
	o, err := url.Parse(origin)
	return err == nil && u.Scheme == o.Scheme && u.Host == o.Host
}

func normalizeBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("--url must be an https URL without credentials, query or fragment")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String(), nil
}

// validPath accepts a relative path of plain segments (none starts with a
// dot): it cannot leave the source base or name another origin.
func validPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if !pathSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

func parseReleaseTag(tag string) (line, version string, ok bool) {
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		return "", "", false
	}
	v, err := semver.StrictNewVersion(m[2])
	if err != nil || v.Prerelease() != "" || v.Metadata() != "" {
		return "", "", false
	}
	return m[1], m[2], true
}

// compareReleases orders by line, then newest version first.
func compareReleases(a, b indexRelease) int {
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
func mergeIndex(path string, entries []indexRelease) error {
	idx := releaseIndex{Schema: releasesSchema}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err = json.Unmarshal(data, &idx); err != nil || idx.Schema != releasesSchema {
			return fmt.Errorf("%s is not a %s index", path, releasesSchema)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	for _, e := range entries {
		idx.Releases = slices.DeleteFunc(idx.Releases, func(o indexRelease) bool { return o.Line == e.Line && o.Version == e.Version })
		idx.Releases = append(idx.Releases, e)
	}
	slices.SortStableFunc(idx.Releases, compareReleases)
	if data, err = json.MarshalIndent(idx, "", "  "); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".releases-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
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
