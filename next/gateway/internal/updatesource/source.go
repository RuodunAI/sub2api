// Package updatesource lists the signed releases of the core and gateway
// lines and opens their assets, from GitHub Releases or from an HTTPS release
// directory indexed by releases.json. It only transports bytes: the caller
// verifies every manifest signature and every digest.
package updatesource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

type Kind string

const (
	GitHub Kind = "github"
	URL    Kind = "url"
)

// Release lines and their tag prefixes ("core-v1.2.3").
const (
	LineCore    = "core"
	LineGateway = "gateway"
)

// DefaultRepository is the official release repository.
const DefaultRepository = "Sub2API-Devs/sup2api"

// MaxListingBytes bounds a release list or index.
const MaxListingBytes = 2 << 20

// metadataTimeout bounds one listing request.
const metadataTimeout = 30 * time.Second

type Config struct {
	Enabled    bool
	Kind       Kind
	Repository string // github: normalized "owner/repo"
	URL        string // url: normalized https base ending in "/"
	Secret     string // plaintext, only in memory
	Revision   int64
}

// Listing is one stable release of one line.
type Listing struct {
	Line        string
	Version     string // semantic version without prefix
	Tag         string
	PublishedAt time.Time
	Notes       string
	Page        string // human page, https only
	Assets      map[string]Asset
}

type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Ref  string `json:"ref"` // github: numeric asset id; url: path relative to the index
}

type Source interface {
	// Releases lists the stable releases of both lines, newest first per line.
	Releases(ctx context.Context) ([]Listing, error)
	// Open streams one asset of at most max bytes. The secret goes only to the
	// API host (github) or the configured origin (url).
	Open(ctx context.Context, a Asset, max int64) (io.ReadCloser, error)
}

type Options struct {
	// AllowPrivate lets a url source resolve to private addresses.
	AllowPrivate bool
	// Client replaces Client(...) in tests.
	Client *http.Client
}

func New(cfg Config, opts Options) (Source, error) {
	client := opts.Client
	switch cfg.Kind {
	case GitHub:
		repo, err := NormalizeRepository(cfg.Repository)
		if err != nil || repo != cfg.Repository {
			return nil, errors.New("invalid update source repository")
		}
		if client == nil {
			client = Client(false)
		}
		return &github{repo: repo, secret: cfg.Secret, client: client}, nil
	case URL:
		base, err := NormalizeURL(cfg.URL)
		if err != nil || base != cfg.URL {
			return nil, errors.New("invalid update source URL")
		}
		if client == nil {
			client = Client(opts.AllowPrivate)
		}
		u, _ := url.Parse(base)
		return &index{base: u, secret: cfg.Secret, client: client}, nil
	}
	return nil, errors.New("unknown update source kind")
}

// Latest is the newest listing of line.
func Latest(ls []Listing, line string) (Listing, bool) {
	var best Listing
	var bestVersion *semver.Version
	for _, l := range ls {
		v, err := semver.StrictNewVersion(l.Version)
		if l.Line != line || err != nil {
			continue
		}
		if bestVersion == nil || v.GreaterThan(bestVersion) {
			best, bestVersion = l, v
		}
	}
	return best, bestVersion != nil
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// NormalizeRepository accepts owner/repo or an HTTPS github.com repository URL.
func NormalizeRepository(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("repository must be a github.com owner/repository")
		}
		raw = strings.Trim(u.Path, "/")
	}
	raw = strings.TrimSuffix(raw, ".git")
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || !repositoryPart.MatchString(parts[0]) || !repositoryPart.MatchString(parts[1]) {
		return "", errors.New("repository must be owner/repository or an HTTPS github.com repository URL")
	}
	return parts[0] + "/" + parts[1], nil
}

// NormalizeURL accepts an HTTPS release directory without credentials, query
// or fragment, and returns it ending in "/".
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("url must be an HTTPS release directory without credentials or query")
	}
	u.Host = strings.ToLower(u.Host)
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		u.RawPath = ""
	}
	if len(u.String()) > 2048 {
		return "", errors.New("url is too long")
	}
	return u.String(), nil
}

// parseVersion accepts a stable semantic version: strict, no pre-release.
func parseVersion(raw string) (string, bool) {
	v, err := semver.StrictNewVersion(raw)
	return raw, err == nil && v.Prerelease() == "" && len(raw) <= 64
}

// lineOf splits a "<line>-v<version>" tag.
func lineOf(tag string) (line, version string, ok bool) {
	for _, l := range []string{LineCore, LineGateway} {
		if v, found := strings.CutPrefix(tag, l+"-v"); found {
			version, ok = parseVersion(v)
			return l, version, ok
		}
	}
	return "", "", false
}

// sortListings orders newest first within each line.
func sortListings(ls []Listing) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].Line != ls[j].Line {
			return ls[i].Line < ls[j].Line
		}
		return semver.MustParse(ls[i].Version).GreaterThan(semver.MustParse(ls[j].Version))
	})
}

// validAssetName accepts a plain file name.
func validAssetName(name string) bool {
	return name != "" && len(name) <= 255 && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

const maxRedirects = 3

var errRedirect = errors.New("redirect rejected")

// get sends one GET and returns its 200 response. redirect decides each
// redirect (nil refuses all). Errors never carry a URL: a redirect target
// may hold a signed query.
func get(ctx context.Context, client *http.Client, raw string, header http.Header, redirect func(*http.Request) error) (*http.Response, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if redirect == nil || len(via) > maxRedirects {
			return errRedirect
		}
		return redirect(req)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, errors.New("invalid update source request")
	}
	req.Header = header
	setDefaultHeaders(req.Header)
	res, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, statusError(res)
	}
	return res, nil
}

func setDefaultHeaders(h http.Header) {
	h.Set("User-Agent", "sub2api-gateway-updater")
	// Sizes are of the stored bytes; no transparent decompression.
	h.Set("Accept-Encoding", "identity")
}

func statusError(res *http.Response) error {
	switch {
	case res.StatusCode == http.StatusTooManyRequests || (res.StatusCode == http.StatusForbidden && res.Header.Get("X-RateLimit-Remaining") == "0"):
		return errors.New("rate limited by the update source; try again later or configure a token")
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("HTTP %d: the update source refused the credentials", res.StatusCode)
	case res.StatusCode == http.StatusNotFound:
		return errors.New("HTTP 404: not found (a private source needs a token)")
	}
	return fmt.Errorf("HTTP %d", res.StatusCode)
}

// readAll reads at most max bytes of r.
func readAll(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return b, nil
}

// limited fails the read past max bytes instead of truncating silently.
type limited struct {
	body io.ReadCloser
	left int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, errors.New("asset exceeds its size limit")
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.body.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, errors.New("asset exceeds its size limit")
	}
	return n, err
}
func (l *limited) Close() error { return l.body.Close() }

// openAsset sends the asset request and bounds the body to max bytes.
func openAsset(ctx context.Context, client *http.Client, raw string, header http.Header, redirect func(*http.Request) error, a Asset, max int64) (io.ReadCloser, error) {
	if a.Size > max {
		return nil, fmt.Errorf("asset %s exceeds %d bytes", a.Name, max)
	}
	res, err := get(ctx, client, raw, header, redirect)
	if err != nil {
		return nil, fmt.Errorf("asset %s: %w", a.Name, err)
	}
	if res.ContentLength > max {
		res.Body.Close()
		return nil, fmt.Errorf("asset %s exceeds %d bytes", a.Name, max)
	}
	return &limited{body: res.Body, left: max}, nil
}

// bearer is the request header carrying the source secret, if any.
func bearer(secret string, accept string) http.Header {
	h := http.Header{}
	if accept != "" {
		h.Set("Accept", accept)
	}
	if secret != "" {
		h.Set("Authorization", "Bearer "+secret)
	}
	return h
}

// httpsPage keeps a release page only when it is a plain HTTPS link.
func httpsPage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return ""
	}
	return u.String()
}
