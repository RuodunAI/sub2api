package providerresources

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) BeginSkillDelete(ctx context.Context, owner core.ResourceOwner, parentID, selector string) (out core.SkillDeleteIntent, err error) {
	if !validOwner(owner) || !bounded(parentID) || selector == "latest" {
		return out, core.ErrInvalidArgument
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if e := lockOwner(ctx, tx, owner); e != nil {
			return e
		}
		g := store.G(ctx, tx)
		p, e := skillParent(ctx, tx, owner, parentID, true)
		if e != nil {
			return e
		}
		out.Parent = p
		op := uuid.NewString()
		deleting := map[string]any{"state": "deleting", "operation_id": op}
		if selector == "" {
			var busy bool
			if e = busyVersions(g, p.PublicID).Row().Scan(&busy); e != nil {
				return e
			}
			if busy {
				return core.ErrConflict.WithMessage("skill has an unfinished version mutation")
			}
			e = update(g, resourcesTable, p.PublicID, deleting).Error
		} else {
			v, x := readSkillVersion(g, p, `parent_id=? AND (public_id=? OR legacy_epoch=?) AND state='ready'`, p.PublicID, selector, selector)
			if store.IsNoRows(x) {
				return core.ErrNotFound
			}
			if x != nil {
				return x
			}
			e = update(g, versionsTable, v.PublicVersionID, deleting).Error
			v.State = "deleting"
			v.OperationID = op
			out.Version = &v
		}
		if e != nil {
			return e
		}
		out.OperationID = op
		out.Dispatch = true
		return nil
	})
	return
}

// busyVersions reads whether a version of the skill parentID has an
// unfinished mutation.
func busyVersions(db *gorm.DB, parentID string) *gorm.DB {
	return db.Raw(`SELECT EXISTS(SELECT 1 FROM provider_skill_versions WHERE parent_id=? AND state IN('pending','uncertain','deleting','delete_uncertain'))`, parentID)
}

func (s *Service) FinishSkillDelete(ctx context.Context, in core.SkillDeleteCompletion) error {
	if !validOwner(in.Owner) || !bounded(in.ParentID) || !bounded(in.OperationID) || (in.Outcome != "deleted" && in.Outcome != "uncertain" && (in.Outcome != "rejected" || !confirmedSkillRejection(in.EvidenceCode))) {
		return core.ErrInvalidArgument
	}
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		if e := lockOwner(ctx, tx, in.Owner); e != nil {
			return e
		}
		g := store.G(ctx, tx)
		p, e := skillParent(ctx, tx, in.Owner, in.ParentID, false)
		if e != nil {
			return e
		}
		table, id, state, op := resourcesTable, p.PublicID, p.State, p.OperationID
		if in.PublicVersionID != "" {
			v, x := readSkillVersion(g, p, `parent_id=? AND public_id=?`, p.PublicID, in.PublicVersionID)
			if x != nil {
				return x
			}
			table, id, state, op = versionsTable, v.PublicVersionID, v.State, v.OperationID
		}
		if op != in.OperationID {
			return core.ErrConflict
		}
		target := "delete_uncertain"
		if in.Outcome == "deleted" {
			target = "deleted"
		}
		if in.Outcome == "rejected" {
			target = "ready"
		}
		if state == target {
			return nil
		}
		if state != "deleting" && state != "delete_uncertain" {
			return core.ErrConflict
		}
		if e = update(g, table, id, map[string]any{"state": target}).Error; e != nil {
			return e
		}
		if in.PublicVersionID == "" && target == "deleted" {
			e = deleteVersions(g, p.PublicID).Error
		}
		return e
	})
}

// deleteVersions marks every version of the skill parentID deleted.
func deleteVersions(db *gorm.DB, parentID string) *gorm.DB {
	return db.Model(&model.ProviderSkillVersion{}).Where("parent_id = ?", parentID).
		Updates(map[string]any{"state": "deleted", "updated_at": gorm.Expr("now()")})
}
