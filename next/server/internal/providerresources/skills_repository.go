package providerresources

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var _ core.SkillResources = (*Service)(nil)

const skillVersionColumns = `public_id,remote_id,legacy_epoch,state,operation_id,bytes,metadata,created_at`

// skillVersions selects the columns of provider_skill_versions a
// SkillVersion holds.
func skillVersions(db *gorm.DB) *gorm.DB {
	return db.Model(&model.ProviderSkillVersion{}).Select(skillVersionColumns)
}

// skillVersion is the SkillVersion of parent a row holds.
func skillVersion(parent core.ProviderResource, row model.ProviderSkillVersion) core.SkillVersion {
	return core.SkillVersion{PublicVersionID: row.PublicID, RemoteVersionID: row.RemoteID, LegacyEpoch: row.LegacyEpoch, State: row.State,
		OperationID: row.OperationID, Parent: parent, Bytes: row.Bytes, Metadata: row.Metadata, CreatedAt: row.CreatedAt}
}

// readSkillVersion reads the first skill version of parent that cond (with
// args) selects, in the order db asks for.
func readSkillVersion(db *gorm.DB, parent core.ProviderResource, cond string, args ...any) (core.SkillVersion, error) {
	var row model.ProviderSkillVersion
	err := skillVersions(db).Where(cond, args...).Take(&row).Error
	return skillVersion(parent, row), err
}
func skillParent(ctx context.Context, q *gorm.DB, owner core.ResourceOwner, id string, ready bool) (core.ProviderResource, error) {
	r, _, err := read(store.G(ctx, q), `user_id=? AND group_id=? AND public_id=?`, owner.UserID, owner.GroupID, id)
	if store.IsNoRows(err) {
		return r, core.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if r.Kind != "skill" || r.PluginKey != "ccgateway" || (ready && r.State != "ready") {
		return core.ProviderResource{}, core.ErrNotFound
	}
	return r, nil
}
func (s *Service) FindVersion(ctx context.Context, owner core.ResourceOwner, parentID, selector string) (core.SkillVersion, error) {
	return s.findSkillVersion(ctx, owner, parentID, selector, false)
}
func (s *Service) FindObservedVersion(ctx context.Context, owner core.ResourceOwner, parentID, selector string) (core.SkillVersion, error) {
	if selector == "latest" {
		return core.SkillVersion{}, core.ErrInvalidArgument
	}
	return s.findSkillVersion(ctx, owner, parentID, selector, true)
}
func (s *Service) findSkillVersion(ctx context.Context, owner core.ResourceOwner, parentID, selector string, providerSelector bool) (core.SkillVersion, error) {
	if !validOwner(owner) || !bounded(parentID) || !bounded(selector) {
		return core.SkillVersion{}, core.ErrInvalidArgument
	}
	var v core.SkillVersion
	err := s.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		parent, err := skillParent(ctx, tx, owner, parentID, true)
		if err != nil {
			return err
		}
		g := store.G(ctx, tx)
		if providerSelector {
			v, err = readSkillVersion(g, parent, `parent_id=? AND state='ready' AND (remote_id=? OR legacy_epoch=?)`, parentID, selector, selector)
		} else if selector == "latest" {
			v, err = readSkillVersion(g.Order(newestFirst), parent, `parent_id=? AND state='ready'`, parentID)
		} else {
			v, err = readSkillVersion(g, parent, `parent_id=? AND state='ready' AND (public_id=? OR legacy_epoch=?)`, parentID, selector, selector)
		}
		if store.IsNoRows(err) {
			err = core.ErrNotFound
		}
		return err
	})
	if err != nil {
		return core.SkillVersion{}, err
	}
	return v, nil
}
func (s *Service) ListSkillVersions(ctx context.Context, owner core.ResourceOwner, parentID, after string, limit int) (out core.SkillVersionPage, err error) {
	if limit < 1 || limit > 1000 || !validOwner(owner) || !bounded(parentID) || (after != "" && !bounded(after)) {
		return out, core.ErrInvalidArgument
	}
	err = s.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		p, err := skillParent(ctx, tx, owner, parentID, true)
		if err != nil {
			return err
		}
		g := store.G(ctx, tx)
		var before *core.SkillVersion
		if after != "" {
			v, e := readSkillVersion(g, p, `parent_id=? AND public_id=? AND state='ready'`, parentID, after)
			if store.IsNoRows(e) {
				return core.ErrNotFound
			}
			if e != nil {
				return e
			}
			before = &v
		}
		var rows []model.ProviderSkillVersion
		if err := readyVersions(g, parentID, before, limit+1).Find(&rows).Error; err != nil {
			return err
		}
		out.Items = make([]core.SkillVersion, len(rows))
		for i, row := range rows {
			out.Items[i] = skillVersion(p, row)
		}
		return nil
	})
	if err != nil {
		return core.SkillVersionPage{}, err
	}
	out.HasMore = len(out.Items) > limit
	if out.HasMore {
		out.Items = out.Items[:limit]
	}
	return out, nil
}

// newestFirst orders skill versions newest first, public IDs breaking ties.
const newestFirst = "created_at DESC,public_id DESC"

// readyVersions selects up to limit ready versions of parentID, newest
// first, after the version before (nil: from the newest).
func readyVersions(db *gorm.DB, parentID string, before *core.SkillVersion, limit int) *gorm.DB {
	db = skillVersions(db).Where(`parent_id=? AND state='ready'`, parentID)
	if before != nil {
		db = db.Where(`(created_at,public_id)<(?,?)`, before.CreatedAt, before.PublicVersionID)
	}
	return db.Order(newestFirst).Limit(limit)
}

func (s *Service) ListSkills(ctx context.Context, owner core.ResourceOwner, accounts []int64, after string, limit int) (core.ResourcePage, error) {
	return s.query(ctx, owner, core.ResourceQuery{PluginKey: "ccgateway", Kind: "skill", AccountIDs: accounts, AfterID: after, Limit: limit, ReadyOnly: true}, true)
}
