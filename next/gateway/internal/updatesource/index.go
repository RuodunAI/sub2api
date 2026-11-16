package updatesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IndexSchema names the releases.json format of a url source.
const IndexSchema = "sub2api-releases-v1"

// index is a url source: an HTTPS directory with releases.json.
type index struct {
	base   *url.URL
	secret string
	client *http.Client
}

type indexFile struct {
	Schema   string `json:"schema"`
	Releases []struct {
		Line        string    `json:"line"`
		Version     string    `json:"version"`
		PublishedAt time.Time `json:"published_at"`
		Notes       string    `json:"notes"`
		Page        string    `json:"page"`
		Assets      map[string]struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"assets"`
	} `json:"releases"`
}

func (s *index) Releases(ctx context.Context) ([]Listing, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	res, err := get(ctx, s.client, s.base.String()+"releases.json", bearer(s.secret, "application/json"), s.sameOrigin)
	if err != nil {
		return nil, fmt.Errorf("releases.json: %w", err)
	}
	defer res.Body.Close()
	body, err := readAll(res.Body, MaxListingBytes)
	if err != nil {
		return nil, fmt.Errorf("releases.json: %w", err)
	}
	var f indexFile
	if err = json.Unmarshal(body, &f); err != nil || f.Schema != IndexSchema {
		return nil, errors.New("releases.json: not a " + IndexSchema + " index")
	}
	out := []Listing{}
	seen := map[string]bool{}
	for _, r := range f.Releases {
		// Other lines and pre-releases are not for this gateway.
		tag := r.Line + "-v" + r.Version
		line, version, ok := lineOf(tag)
		if !ok || line != r.Line {
			continue
		}
		if seen[tag] {
			return nil, fmt.Errorf("releases.json: duplicate release %s", tag)
		}
		seen[tag] = true
		l := Listing{Line: line, Version: version, Tag: tag, PublishedAt: r.PublishedAt, Notes: r.Notes, Page: httpsPage(r.Page), Assets: map[string]Asset{}}
		for name, a := range r.Assets {
			if !validAssetName(name) || a.Size <= 0 {
				return nil, fmt.Errorf("releases.json: invalid asset in %s", tag)
			}
			if _, err := s.resolve(a.Path); err != nil {
				return nil, fmt.Errorf("releases.json: asset %.80q of %s: %w", name, tag, err)
			}
			l.Assets[name] = Asset{Name: name, Size: a.Size, Ref: a.Path}
		}
		out = append(out, l)
	}
	sortListings(out)
	return out, nil
}

func (s *index) Open(ctx context.Context, a Asset, max int64) (io.ReadCloser, error) {
	u, err := s.resolve(a.Ref)
	if err != nil {
		return nil, err
	}
	return openAsset(ctx, s.client, u.String(), bearer(s.secret, ""), s.sameOrigin, a, max)
}

// resolve maps an index path to its URL below the base. The path must be
// relative, without "." or ".." segments, query or fragment.
func (s *index) resolve(ref string) (*url.URL, error) {
	invalid := errors.New("asset path must be a relative path below the index")
	if ref == "" || len(ref) > 1024 || strings.ContainsAny(ref, "\\?#\x00") || strings.HasPrefix(ref, "/") {
		return nil, invalid
	}
	r, err := url.Parse(ref)
	if err != nil || r.Scheme != "" || r.Host != "" || r.User != nil || r.Opaque != "" {
		return nil, invalid
	}
	for _, seg := range strings.Split(r.Path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, invalid
		}
	}
	u := s.base.ResolveReference(r)
	if !s.origin(u) || !strings.HasPrefix(u.Path, s.base.Path) {
		return nil, invalid
	}
	return u, nil
}

func (s *index) origin(u *url.URL) bool {
	return u.Scheme == s.base.Scheme && u.Host == s.base.Host && u.User == nil
}

// sameOrigin follows a redirect only within the configured origin, where the
// secret may go.
func (s *index) sameOrigin(req *http.Request) error {
	if !s.origin(req.URL) {
		return errors.New("cross-origin redirect rejected")
	}
	return nil
}
