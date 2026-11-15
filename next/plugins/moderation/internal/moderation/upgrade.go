package moderation

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"gorm.io/gorm"
)

// upgradeOnce runs the one-time steps of the 0.11.0 upgrade (CONTRACTS §72
// 0.11.0) once the real settings are in (configured):
//   - the locked watch entries become the user lists of the settings, once
//     per installation (config_state.lists_imported; importLists). From then on
//     the watch list is the only source and the lists are never read again.
//   - a 0.1 single-model setup that was never converted (config_state.
//     legacy_converted) is logged: 0.11.0 no longer converts it.
//
// A failed import is retried on the next refresh.
func (p *Plugin) upgradeOnce(ctx context.Context) {
	if p.upgraded.Load() || p.host == nil || !p.configured.Load() {
		return
	}
	db, err := p.db(ctx)
	if err != nil {
		return
	}
	c := p.cfg.Load()
	var imported, converted bool
	if err := queryRow(db, `SELECT lists_imported, legacy_converted FROM config_state WHERE id = 1`).Scan(&imported, &converted); err != nil {
		p.log.Warn("moderation: reading the upgrade state failed, retrying later", "error", err.Error())
		return
	}
	if !imported {
		fixed, err := p.importLists(db, c.lists)
		if err == nil {
			// Another node may have set it first: setting it again is harmless.
			err = db.Exec(`UPDATE config_state SET lists_imported = true WHERE id = 1`).Error
		}
		if fixed > 0 {
			p.watchChanged(ctx)
		}
		if err != nil {
			p.log.Warn("moderation: importing the user lists of the settings failed, retrying later", "error", err.Error())
			return
		}
		p.log.Info("moderation: imported the user lists of the settings into the watch list",
			"allowlist", len(c.lists.allow), "blocklist", len(c.lists.block), "invalid", c.lists.invalid, "fixed", fixed)
	}
	if !converted && c.setup01 {
		p.log.Error("moderation: the settings hold a 0.1 single-model setup that was never converted, and 0.11.0 no longer converts it: upgrade through moderation <= 0.10 first")
	}
	p.upgraded.Store(true)
}

// importLists makes the locked entries equal l: every user of l gets a
// locked entry of their list (the blocklist wins), and the locked entries of
// users on neither list are removed, each user in its own transaction under
// the user's lock, so nodes importing at the same time fix each entry once.
// Every fix writes a history row and a log line. fixed counts the fixes,
// also when it stops at an error.
func (p *Plugin) importLists(db *gorm.DB, l retiredLists) (fixed int, err error) {
	locked, err := column[int64](db.Raw(`SELECT user_id FROM watchlist WHERE locked`))
	if err != nil {
		return 0, err
	}
	lists := map[int64]string{} // "" = neither list
	for _, uid := range locked {
		lists[uid] = ""
	}
	for _, uid := range l.allow {
		lists[uid] = LockAllowlist
	}
	for _, uid := range l.block {
		lists[uid] = LockBlocklist
	}
	now := p.now().UTC()
	for _, uid := range slices.Sorted(maps.Keys(lists)) {
		was, changed, err := importEntry(db, uid, lists[uid], now)
		if err != nil {
			return fixed, err
		}
		if changed {
			fixed++
			p.log.Info("moderation: list import fixed a watch entry", "user_id", uid, "list", cmp.Or(lists[uid], "none"), "was", was)
		}
	}
	return fixed, nil
}

// importEntry makes uid's entry a locked entry of list, or, for list "",
// removes uid's locked entry; was describes the entry before (changed = it
// wrote).
func importEntry(db *gorm.DB, uid int64, list string, now time.Time) (was string, changed bool, err error) {
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockBlockSubject(tx, uid); err != nil {
			return err
		}
		w, found, expired, err := liveWatchEntry(tx, uid, now)
		if err != nil {
			return err
		}
		was = entryState(w, found)
		if list == "" {
			changed, err = importUnlock(tx, w, found)
		} else {
			_, changed, err = lockEntry(tx, w, found, list, now, watchChange{uid: uid, reason: lockLists[list].imported})
		}
		if err != nil || !(changed || expired) {
			return err
		}
		return bumpWatch(tx)
	})
	return was, changed && err == nil, err
}

// importUnlock removes w, the entry of a user on neither list, if it is
// locked (changed = it did).
func importUnlock(tx *gorm.DB, w Watch, found bool) (bool, error) {
	if !found || !w.Locked {
		return false, nil
	}
	return true, unlockEntry(tx, w, lockLists[w.Lock].unlisted, nil)
}

// entryState describes an entry for the log: "none", or its level, whether
// it is locked and its expiry.
func entryState(w Watch, found bool) string {
	if !found {
		return "none"
	}
	s := w.Level
	if w.Locked {
		s = "locked " + s
	}
	if w.ExpiresAt != nil {
		s += " until " + w.ExpiresAt.Format(time.RFC3339)
	}
	return s
}
