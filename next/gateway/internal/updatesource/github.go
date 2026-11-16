package updatesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
)

const githubAPI = "https://api.github.com/repos/"

// GitHub lists releases newest first, githubPerPage at a time, drafts,
// pre-releases and both lines together. A listing reads at most
// githubMaxPages pages, so that Apply finds any version among the newest
// MaxGitHubReleases releases.
const (
	githubPerPage     = 50
	githubMaxPages    = 10
	MaxGitHubReleases = githubPerPage * githubMaxPages
)

// githubAssetHosts are the CDNs GitHub redirects release asset downloads to.
var githubAssetHosts = map[string]bool{"release-assets.githubusercontent.com": true, "objects.githubusercontent.com": true}

type github struct {
	repo, secret string
	client       *http.Client
}

type githubRelease struct {
	Tag         string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// header carries the token only to api.github.com: every request goes there,
// and an asset redirect starts over with clean headers (assetRedirect).
func (g *github) header(accept string) http.Header {
	h := bearer(g.secret, accept)
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	return h
}

// Releases reads pages until one is short or githubMaxPages were read, all
// within metadataTimeout. A release that moved to the next page while paging
// is listed once.
func (g *github) Releases(ctx context.Context) ([]Listing, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	out := []Listing{}
	seen := map[string]bool{}
	for page := 1; page <= githubMaxPages; page++ {
		raw, err := g.releasePage(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("GitHub releases: %w", err)
		}
		for _, r := range raw {
			line, version, ok := lineOf(r.Tag)
			if r.Draft || r.Prerelease || !ok || seen[r.Tag] {
				continue
			}
			seen[r.Tag] = true
			out = append(out, g.listing(r, line, version))
		}
		if len(raw) < githubPerPage {
			break
		}
	}
	sortListings(out)
	return out, nil
}

// releasePage reads one page of the release list. Its URL is built here,
// never taken from a response (no Link header, no redirect), so the token
// only ever goes to api.github.com.
func (g *github) releasePage(ctx context.Context, page int) ([]githubRelease, error) {
	res, err := get(ctx, g.client, fmt.Sprintf("%s%s/releases?per_page=%d&page=%d", githubAPI, g.repo, githubPerPage, page), g.header("application/vnd.github+json"), nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := readAll(res.Body, MaxListingBytes)
	if err != nil {
		return nil, err
	}
	var raw []githubRelease
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("invalid response")
	}
	return raw, nil
}

func (g *github) listing(r githubRelease, line, version string) Listing {
	l := Listing{Line: line, Version: version, Tag: r.Tag, PublishedAt: r.PublishedAt, Notes: r.Body,
		Page: "https://github.com/" + g.repo + "/releases/tag/" + url.PathEscape(r.Tag), Assets: map[string]Asset{}}
	for _, a := range r.Assets {
		if release.ValidFileName(a.Name) && a.ID > 0 {
			l.Assets[a.Name] = Asset{Name: a.Name, Size: a.Size, Ref: strconv.FormatInt(a.ID, 10)}
		}
	}
	return l
}

func (g *github) Open(ctx context.Context, a Asset, max int64) (io.ReadCloser, error) {
	id, err := strconv.ParseInt(a.Ref, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != a.Ref {
		return nil, errors.New("invalid GitHub asset reference")
	}
	return openAsset(ctx, g.client, githubAPI+g.repo+"/releases/assets/"+a.Ref, g.header("application/octet-stream"), assetRedirect, a, max)
}

// assetRedirect follows GitHub's asset download only to its release CDNs,
// over HTTPS on the default port, with none of the API request's headers.
func assetRedirect(req *http.Request) error {
	u := req.URL
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || !githubAssetHosts[u.Host] {
		return errors.New("GitHub asset redirect rejected")
	}
	req.Header = http.Header{"Accept": {"application/octet-stream"}}
	setDefaultHeaders(req.Header)
	return nil
}
