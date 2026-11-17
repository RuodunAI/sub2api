package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// settingsTTL bounds how long a section read from the host is used without
// hearing of a change: the core's own settings caches live as long, so a
// process that missed a SettingsChanged call (a lost broadcast) is never
// staler than the core.
const settingsTTL = 10 * time.Second

// Settings returns the effective values of one section of the system
// settings (CONTRACTS §106), the section of T:
//
//	sticky, err := pluginsdk.Settings[protocol.StickySettings](ctx, host)
//
// It needs no grant. A section is read from the host once and kept in the
// process until the host says it changed (PluginService.SettingsChanged,
// sent by every node, which the SDK handles) or settingsTTL passed, so call
// it wherever the value is needed instead of keeping a copy. A plugin that
// must act once when a section changes subscribes to the settings.changed
// event instead.
func Settings[T protocol.SettingsValues](ctx context.Context, h Host) (T, error) {
	var v T
	if err := requireHost(h); err != nil {
		return v, err
	}
	section := v.SettingsSection()
	cache := settingsCacheOf(h)
	raw, epoch, ok := cache.get(section)
	if !ok {
		resp, err := h.Client().GetSettings(ctx, &pluginv1.GetSettingsRequest{Sections: []string{section}})
		if err != nil {
			return v, err
		}
		got := resp.GetSections()
		if len(got) != 1 || got[0].GetName() != section {
			return v, fmt.Errorf("pluginsdk: host answered no %q settings", section)
		}
		raw = []byte(got[0].GetValuesJson())
		cache.put(section, raw, epoch)
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
	entries map[string]settingsEntry
}

type settingsEntry struct {
	at  time.Time
	raw []byte
}

func settingsCacheOf(h Host) *settingsCache {
	if sh, ok := h.(*host); ok {
		return &sh.settings
	}
	return nil
}

// get returns the cached section, or the epoch a read must carry to put.
func (c *settingsCache) get(section string) ([]byte, uint64, bool) {
	if c == nil {
		return nil, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[section]
	if !ok || time.Since(e.at) >= settingsTTL {
		return nil, c.epoch, false
	}
	return e.raw, c.epoch, true
}

func (c *settingsCache) put(section string, raw []byte, epoch uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return
	}
	if c.entries == nil {
		c.entries = map[string]settingsEntry{}
	}
	c.entries[section] = settingsEntry{at: time.Now(), raw: raw}
}

// drop forgets the sections, all of them when none is named.
func (c *settingsCache) drop(sections []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for section := range c.entries {
		if len(sections) == 0 || slices.Contains(sections, section) {
			delete(c.entries, section)
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
