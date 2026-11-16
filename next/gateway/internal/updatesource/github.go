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
)

const githubAPI = "https://api.github.com/repos/"

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

func (g *github) Releases(ctx context.Context) ([]Listing, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	res, err := get(ctx, g.client, githubAPI+g.repo+"/releases?per_page=50", g.header("application/vnd.github+json"), nil)
	if err != nil {
		return nil, fmt.Errorf("GitHub releases: %w", err)
	}
	defer res.Body.Close()
	body, err := readAll(res.Body, MaxListingBytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub releases: %w", err)
	}
	var raw []githubRelease
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("GitHub releases: invalid response")
	}
	out := []Listing{}
	for _, r := range raw {
		line, version, ok := lineOf(r.Tag)
		if r.Draft || r.Prerelease || !ok {
			continue
		}
		l := Listing{Line: line, Version: version, Tag: r.Tag, PublishedAt: r.PublishedAt, Notes: r.Body,
			Page: "https://github.com/" + g.repo + "/releases/tag/" + url.PathEscape(r.Tag), Assets: map[string]Asset{}}
		for _, a := range r.Assets {
			if validAssetName(a.Name) && a.ID > 0 {
				l.Assets[a.Name] = Asset{Name: a.Name, Size: a.Size, Ref: strconv.FormatInt(a.ID, 10)}
			}
		}
		out = append(out, l)
	}
	sortListings(out)
	return out, nil
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
