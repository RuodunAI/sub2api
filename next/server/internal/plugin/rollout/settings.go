package rollout

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// settingsReceiver is implemented by instances whose plugin process can be
// told that system settings changed (grpcruntime.Instance).
type settingsReceiver interface {
	SettingsChanged(ctx context.Context, sections []string) error
}

// onConfigChanged hands a settings section saved anywhere in the cluster to
// every plugin instance loaded on this node, a rollout's included
// (PluginService.SettingsChanged, CONTRACTS §107). Every node receives config:changed, the writer's own
// included, so every plugin process learns of the change; the
// settings.changed event reaches one instance per plugin. It runs on the
// bus goroutine, so the calls go out on their own.
func (c *Controller) onConfigChanged(payload []byte) {
	section := changedSettingsSection(payload)
	if section == "" {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		sections := []string{section}
		var wg sync.WaitGroup
		for _, ki := range c.loadedInstances() {
			r, ok := ki.inst.(settingsReceiver)
			if !ok {
				continue
			}
			wg.Go(func() {
				if err := r.SettingsChanged(c.ctx, sections); err != nil {
					c.log.Warn("plugin settings change not delivered", "plugin", ki.key, "section", section, "err", err)
				}
			})
		}
		wg.Wait()
	}()
}

// changedSettingsSection is the plugin-visible section a config:changed
// message names ({"key":"settings.<section>"}), "" for any other message.
func changedSettingsSection(payload []byte) string {
	var msg struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(payload, &msg) != nil {
		return ""
	}
	section, ok := strings.CutPrefix(msg.Key, "settings.")
	if !ok || !slices.Contains(protocol.SettingsSections, section) {
		return ""
	}
	return section
}
