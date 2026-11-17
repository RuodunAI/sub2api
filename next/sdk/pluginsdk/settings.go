package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

const (
	// settingsTTL bounds how long a section read from the host is used
	// without hearing of a change: it only covers a SettingsChanged call
	// that never arrived (a lost broadcast, a call that timed out).
	settingsTTL = 10 * time.Second
	// settingsErrorTTL is how long after a failed read the host is not
	// asked again, unless it says the section changed.
	settingsErrorTTL = 2 * time.Second
	// settingsReadTimeout bounds one read of the host, shared by every
	// caller waiting for it.
	settingsReadTimeout = 5 * time.Second
)

// Settings returns the effective values of one section of the system
// settings (CONTRACTS §107), the section of T:
//
//	sticky, err := pluginsdk.Settings[protocol.StickySettings](ctx, host)
//
// It needs no grant. A section is read from the host once and kept in the
// process until the host says it changed (PluginService.SettingsChanged,
// sent by every node, which the SDK handles) or settingsTTL passed, so call
// it wherever the value is needed, a hot path included, instead of keeping
// a copy. Concurrent callers share one read of the host. When a read fails,
// Settings returns the values last read and asks the host again only after
// settingsErrorTTL or a change; it returns the error only when no read ever
// succeeded. A plugin that must act once when a section changes subscribes
// to the settings.changed event instead.
func Settings[T protocol.SettingsValues](ctx context.Context, h Host) (T, error) {
	var v T
	if err := requireHost(h); err != nil {
		return v, err
	}
	section := v.SettingsSection()
	raw, err := settingsCacheOf(h).load(ctx, section, func(ctx context.Context) ([]byte, error) {
		resp, err := h.Client().GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{section}})
		if err != nil {
			return nil, err
		}
		got := resp.GetSections()
		if len(got) != 1 || got[0].GetName() != section {
			return nil, fmt.Errorf("pluginsdk: host answered no %q settings", section)
		}
		return []byte(got[0].GetValuesJson()), nil
	})
	if err != nil {
		return v, err
	}
	// Decoded on every call: the caller may change what it gets.
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("pluginsdk: decode %q settings: %w", section, err)
	}
	return v, nil
}

// settingsCache holds the sections one process read; the zero value is
// ready and a nil cache keeps nothing (a Host the SDK did not build).
type settingsCache struct {
	mu      sync.Mutex
	epoch   uint64 // bumped by drop, so a read in flight is not kept
	entries map[string]*settingsEntry
	reads   singleflight.Group // by section and epoch
}

type settingsEntry struct {
	raw     []byte    // the last successful read; nil before one
	at      time.Time // when raw was read; zero once dropped
	retryAt time.Time // no read of the host before it, after a failure
	err     error     // that failure, returned while there is no raw
}

func settingsCacheOf(h Host) *settingsCache {
	if sh, ok := h.(*host); ok {
		return &sh.settings
	}
	return nil
}

// load returns the section: the cached values while fresh, else one read
// shared by every caller of the same epoch. A failed read leaves the last
// values in use (or its error, when there are none) for settingsErrorTTL.
func (c *settingsCache) load(ctx context.Context, section string, read func(context.Context) ([]byte, error)) ([]byte, error) {
	if c == nil {
		return read(ctx)
	}
	raw, epoch, done, err := c.cached(section, time.Now())
	if done {
		return raw, err
	}
	ch := c.reads.DoChan(section+"\x00"+strconv.FormatUint(epoch, 10), func() (any, error) {
		// Not the first caller's context: the read serves every caller.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settingsReadTimeout)
		defer cancel()
		raw, err := read(rctx)
		return c.store(section, epoch, raw, err)
	})
	select {
	case r := <-ch:
		raw, _ := r.Val.([]byte)
		return raw, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// cached answers from the entry when it can (done), and gives the epoch a
// read must carry otherwise.
func (c *settingsCache) cached(section string, now time.Time) (raw []byte, epoch uint64, done bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[section]
	switch {
	case e == nil:
	case e.raw != nil && !e.at.IsZero() && now.Sub(e.at) < settingsTTL:
		return e.raw, c.epoch, true, nil
	case now.Before(e.retryAt):
		if e.raw != nil {
			return e.raw, c.epoch, true, nil
		}
		return nil, c.epoch, true, e.err
	}
	return nil, c.epoch, false, nil
}

// store keeps a read made in epoch, unless a drop came since, and returns
// what its callers get: the values read, else the last ones, else err.
func (c *settingsCache) store(section string, epoch uint64, raw []byte, err error) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[section]
	if e == nil {
		e = &settingsEntry{}
		if c.entries == nil {
			c.entries = map[string]*settingsEntry{}
		}
		c.entries[section] = e
	}
	if c.epoch == epoch {
		if err != nil {
			e.retryAt, e.err = time.Now().Add(settingsErrorTTL), err
		} else {
			e.raw, e.at, e.retryAt, e.err = raw, time.Now(), time.Time{}, nil
		}
	}
	if err != nil && e.raw != nil {
		return e.raw, nil
	}
	return raw, err
}

// drop marks the sections stale, all of them when none is named: the next
// load reads the host at once, and keeps the last values for a failure.
func (c *settingsCache) drop(sections []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for section, e := range c.entries {
		if len(sections) == 0 || slices.Contains(sections, section) {
			e.at, e.retryAt, e.err = time.Time{}, time.Time{}, nil
		}
	}
}

// SettingsChanged implements PluginService.SettingsChanged: the next
// Settings call reads the sections from the host again.
func (rt *runtime) SettingsChanged(_ context.Context, in *pluginv1.SettingsChangedRequest) (*pluginv1.SettingsChangedResponse, error) {
	rt.mu.Lock()
	h := rt.host
	rt.mu.Unlock()
	if h != nil {
		h.settings.drop(in.GetSections())
	}
	return &pluginv1.SettingsChangedResponse{}, nil
}
