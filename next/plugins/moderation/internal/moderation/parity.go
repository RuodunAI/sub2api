package moderation

import (
	"context"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	"gorm.io/gorm"
)

// parityReason is the history reason of an entry convertParityOnce changed.
const parityReason = "0.8.0 升级：默认级别为拦截，自动加入的条目改为拦截级 / upgrade to 0.8.0: the default level is enforce, automatic entries become enforce"

// parityWindow is how long after the conversion entries added by nodes still
// running 0.7.x are converted too (a rolling upgrade).
const parityWindow = 24 * time.Hour

// convertParityOnce adapts the watch list to the 0.8.0 timing rules, once
// per installation (config_state.parity_converted). Until 0.8.0 mode =
// enforce moderated everyone first and automatic entries were always added
// at observe; from 0.8.0 an entry's own level wins over the default level,
// so those entries would move their users to moderation after the request.
// Under mode = enforce the live automatic observe entries therefore become
// enforce entries, as they would have been added under the new rules.
// Entries an administrator or the AI set to observe are a choice and stay.
//
// During a rolling upgrade a node still running 0.7.x can add another
// automatic observe entry after the conversion (and every hit renews it), so
// for parityWindow after the first run (config_state.parity_converted_at)
// the automatic observe entries added since are converted as well, under
// mode = enforce as it is then. Entries from before the first run are not
// looked at again: with mode = observe at the first run they stay.
//
// Each entry is changed in its own transaction, in the lock order of every
// other watch change (the user's lock, the entry, config_state), so the
// conversion cannot deadlock with them; the entry's own row lock and the
// conditions of the update make two nodes convert it once.
func (p *Plugin) convertParityOnce(ctx context.Context) {
	if p.host == nil || !p.configured.Load() {
		return
	}
	now := p.now().UTC()
	if p.parityDone.Load() && now.UnixNano() >= p.parityEnd.Load() {
		return
	}
	db, err := p.db(ctx)
	if err != nil {
		return
	}
	c := p.cfg.Load()
	fail := func(err error) {
		p.log.Warn("moderation: converting the watch list for 0.8.0 failed, retrying later", "error", err.Error())
	}
	var st model.ConfigState
	if err := configState(db).Select("parity_converted, parity_converted_at").Take(&st).Error; err != nil {
		fail(err)
		return
	}
	done, at := st.ParityConverted, st.ParityConvertedAt
	finished := func() {
		end := int64(0) // no timestamp: converted before it was recorded, no window
		if at != nil {
			end = at.Add(parityWindow).UnixNano()
		}
		p.parityEnd.Store(end)
		p.parityDone.Store(true)
	}
	var since *time.Time // nil = the first run: every live entry
	if done {
		if at == nil || !now.Before(at.Add(parityWindow)) {
			finished()
			return
		}
		since = at
	}
	converted := 0
	if c.Mode == ModeEnforce {
		converted, err = convertParityEntries(db, now, since)
		if err != nil {
			fail(err)
			if converted > 0 {
				p.watchChanged(ctx)
			}
			return
		}
	}
	if !done {
		// The first run is over; another node may have finished it first,
		// then its time counts.
		err := configState(db).Where("NOT parity_converted").
			Updates(map[string]any{"parity_converted": true, "parity_converted_at": now}).Error
		if err == nil {
			err = configState(db).Select("parity_converted_at").Take(&st).Error
		}
		if err != nil {
			fail(err)
			return
		}
		at = st.ParityConvertedAt
	}
	finished()
	if !done || converted > 0 {
		p.log.Info("moderation: 0.8.0 watch list conversion", "mode", c.Mode, "converted", converted, "first_run", !done)
	}
	if converted > 0 {
		p.watchChanged(ctx)
	}
}

// parityLive selects the entries the conversion changes: the automatic
// observe entries that are live at its one placeholder.
const parityLive = `level = 'observe' AND source = 'auto' AND (expires_at IS NULL OR expires_at > ?)`

// convertParityEntries makes enforce entries of the live automatic observe
// entries added since the given time (nil = all of them), and returns how
// many it changed - also when it stops at an error.
func convertParityEntries(db *gorm.DB, now time.Time, since *time.Time) (converted int, err error) {
	var users []int64
	err = db.Model(&model.Watch{}).Where(parityLive, now).Where("?::timestamptz IS NULL OR since_at >= ?", since, since).
		Order("user_id").Pluck("user_id", &users).Error
	if err != nil {
		return 0, err
	}
	for _, uid := range users {
		changed, err := convertParityEntry(db, now, uid)
		if err != nil {
			return converted, err
		}
		if changed {
			converted++
		}
	}
	return converted, nil
}

// convertParityEntry changes one entry in its own transaction, with a
// history row; false when the entry changed meanwhile or another node
// converted it.
func convertParityEntry(db *gorm.DB, now time.Time, uid int64) (changed bool, err error) {
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockBlockSubject(tx, uid); err != nil {
			return err
		}
		res := tx.Model(&model.Watch{}).Where("user_id = ?", uid).Where(parityLive, now).
			Updates(map[string]any{"level": LevelEnforce, "updated_at": now})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		if err := watchEvent(tx, watchChange{uid: uid, action: "set_level", from: LevelObserve, to: LevelEnforce, reason: parityReason}); err != nil {
			return err
		}
		changed = true
		return bumpWatch(tx)
	})
	return changed && err == nil, err
}
