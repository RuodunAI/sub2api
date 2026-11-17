package pluginsdk

import (
	"testing"
	"time"
)

// A section older than settingsTTL is read again, and a read that started
// before a drop is not kept.
func TestSettingsCacheExpiryAndEpoch(t *testing.T) {
	var c settingsCache
	_, epoch, ok := c.get("sticky")
	if ok {
		t.Fatal("empty cache answered")
	}
	c.put("sticky", []byte(`{}`), epoch)
	if _, _, ok := c.get("sticky"); !ok {
		t.Fatal("fresh section not kept")
	}
	c.entries["sticky"] = settingsEntry{at: time.Now().Add(-settingsTTL), raw: []byte(`{}`)}
	_, epoch, ok = c.get("sticky")
	if ok {
		t.Fatal("expired section answered")
	}
	c.drop([]string{"billing"})
	c.put("sticky", []byte(`{}`), epoch)
	if _, _, ok := c.get("sticky"); ok {
		t.Fatal("a read from before a change was kept")
	}
	var none *settingsCache
	none.put("sticky", nil, 0)
	none.drop(nil)
	if _, _, ok := none.get("sticky"); ok {
		t.Fatal("nil cache answered")
	}
}
