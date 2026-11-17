package rollout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
)

// settingsInst records the sections of SettingsChanged.
type settingsInst struct {
	*stubInst
	sections chan []string
	fail     error
}

func (f settingsInst) SettingsChanged(_ context.Context, sections []string) error {
	f.sections <- sections
	return f.fail
}

// A settings section saved anywhere reaches every plugin instance loaded
// here, a rollout's candidate included; other config:changed messages reach
// none.
func TestConfigChangedNotifiesPlugins(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, &stubRuntime{}, &registrytest.MemBus{}, dir)
	a := settingsInst{newStub(loadPkg(t, dir, "guard", "1.0.0")), make(chan []string, 4), errors.New("plugin error")} // only logged
	b := settingsInst{newStub(loadPkg(t, dir, "dynweight", "1.0.0")), make(chan []string, 4), nil}
	next := settingsInst{newStub(loadPkg(t, dir, "dynweight", "1.1.0")), make(chan []string, 4), nil}
	c.serveForTest(a)
	c.serveForTest(b)
	s := c.slotFor("dynweight") // loaded, not yet in the generation
	s.mu.Lock()
	s.entries["1.1.0"] = &entry{version: "1.1.0", inst: next, epoch: 1}
	s.mu.Unlock()
	c.serveForTest(newStub(loadPkg(t, dir, "plain", "1.0.0"))) // cannot be told

	c.onConfigChanged([]byte(`{"key":"settings.sticky"}`))
	for _, inst := range []settingsInst{a, b, next} {
		select {
		case got := <-inst.sections:
			if len(got) != 1 || got[0] != protocol.SettingsSticky {
				t.Fatalf("%s told %v", inst.pkg.Key, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s not told", inst.pkg.Key)
		}
	}

	for _, payload := range []string{`{"key":"sticky_rules"}`, `{"key":"settings.account-stats"}`, `{"type":"proxy","id":3}`, `{`} {
		c.onConfigChanged([]byte(payload))
	}
	c.wg.Wait()
	select {
	case got := <-a.sections:
		t.Fatalf("told %v for a message that names no section", got)
	default:
	}
}

func TestChangedSettingsSection(t *testing.T) {
	for payload, want := range map[string]string{
		`{"key":"settings.billing"}`:       protocol.SettingsBilling,
		`{"key":"settings.environment"}`:   protocol.SettingsEnvironment,
		`{"key":"settings.reconcile"}`:     "",
		`{"key":"price_rules"}`:            "",
		`{"type":"config","plugin_key":1}`: "",
	} {
		if got := changedSettingsSection([]byte(payload)); got != want {
			t.Fatalf("%s: %q, want %q", payload, got, want)
		}
	}
}
