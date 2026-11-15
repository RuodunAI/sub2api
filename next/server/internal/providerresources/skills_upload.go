package providerresources

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) ReserveSkillUpload(ctx context.Context, input core.SkillUploadIntent) (out core.SkillUploadReservation, err error) {
	normalizer := *s
	normalizer.limits.DefaultTTL = 0
	in, hash, err := normalizer.normalize(input.ResourceIntent)
	if err != nil {
		return out, err
	}
	if in.PluginKey != "ccgateway" || in.Kind != "skill" || in.TTL != 0 || !in.ExpiresAt.IsZero() {
		return out, core.ErrInvalidArgument
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if e := lockOwner(ctx, tx, in.Owner); e != nil {
			return e
		}
		g := store.G(ctx, tx)
		var parent core.ProviderResource
		fresh := false
		if input.ParentID != "" {
			var e error
			parent, e = skillParent(ctx, tx, in.Owner, input.ParentID, true)
			if e != nil {
				return e
			}
			if parent.Binding != in.Binding {
				return core.ErrConflict
			}
		} else {
			var oldHash string
			var e error
			parent, oldHash, e = read(g, `user_id=? AND group_id=? AND request_id=?`, in.Owner.UserID, in.Owner.GroupID, in.RequestID)
			if store.IsNoRows(e) {
				fresh = true
			} else if e != nil {
				return e
			} else if parent.Kind != "skill" || oldHash != hash {
				return core.ErrConflict
			}
		}
		if !fresh {
			v, e := readSkillVersion(g, parent, `parent_id=? AND request_id=?`, parent.PublicID, in.RequestID)
			if e == nil {
				var prior model.ProviderSkillVersion
				if e = g.Select("intent_hash").Where("public_id = ?", v.PublicVersionID).Take(&prior).Error; e != nil {
					return e
				}
				if prior.IntentHash != hash {
					return core.ErrConflict
				}
				out = core.SkillUploadReservation{Parent: parent, Version: v}
				return nil
			}
			if !store.IsNoRows(e) {
				return e
			}
			if input.ParentID == "" {
				return core.ErrConflict
			}
		}
		needed := 1
		if fresh {
			needed++
		}
		if e := s.quota(ctx, tx, in.Owner, needed, in.Bytes); e != nil {
			return e
		}
		op := uuid.NewString()
		if fresh {
			id := "s2res_" + uuid.NewString()
			// The skill takes its bytes from its versions.
			row := newRow(id, op, in, "pending")
			row.Bytes, row.IntentHash = 0, hash
			if e := g.Create(&row).Error; e != nil {
				return e
			}
			var e error
			parent, _, e = read(g, `public_id=?`, id)
			if e != nil {
				return e
			}
		}
		id := "s2skv_" + uuid.NewString()
		e := g.Create(&model.ProviderSkillVersion{PublicID: id, ParentID: parent.PublicID, RequestID: in.RequestID, State: "pending",
			OperationID: op, Bytes: in.Bytes, IntentHash: hash}).Error
		if e != nil {
			return e
		}
		v, e := readSkillVersion(g, parent, `public_id=?`, id)
		if e != nil {
			return e
		}
		out = core.SkillUploadReservation{Parent: parent, Version: v, Dispatch: true}
		return nil
	})
	return
}

func (s *Service) CompleteSkillUpload(ctx context.Context, in core.SkillUploadCompletion) (out core.SkillUploadReservation, err error) {
	if in.CreatedAt.IsZero() || !validOwner(in.Owner) || !validBinding(in.Binding) || !bounded(in.RemoteSkillID) || !bounded(in.RemoteVersionID) || !bounded(in.OperationID) || (in.LegacyEpoch != "" && !bounded(in.LegacyEpoch)) {
		return out, core.ErrInvalidArgument
	}
	if in.LegacyEpoch != "" {
		for _, c := range in.LegacyEpoch {
			if c < '0' || c > '9' {
				return out, core.ErrInvalidArgument
			}
		}
	}
	in.CreatedAt = in.CreatedAt.UTC().Truncate(time.Microsecond)
	vm, err := metadata(in.VersionMetadata)
	if err != nil {
		return out, err
	}
	pm, err := metadata(in.ParentMetadata)
	if err != nil {
		return out, err
	}
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if e := lockOwner(ctx, tx, in.Owner); e != nil {
			return e
		}
		g := store.G(ctx, tx)
		p, e := skillParent(ctx, tx, in.Owner, in.ParentID, false)
		if e != nil {
			return e
		}
		if p.Binding != in.Binding || (p.State != "ready" && p.State != "pending" && p.State != "uncertain") {
			return core.ErrConflict
		}
		v, e := readSkillVersion(g, p, `parent_id=? AND public_id=?`, p.PublicID, in.PublicVersionID)
		if store.IsNoRows(e) {
			return core.ErrNotFound
		}
		if e != nil {
			return e
		}
		if v.OperationID != in.OperationID {
			return core.ErrConflict
		}
		if v.State == "ready" {
			old, _ := metadata(v.Metadata)
			if p.RemoteID != in.RemoteSkillID || !v.CreatedAt.Equal(in.CreatedAt) || v.RemoteVersionID != in.RemoteVersionID || v.LegacyEpoch != in.LegacyEpoch || !bytes.Equal(old, vm) {
				return core.ErrConflict
			}
			out = core.SkillUploadReservation{Parent: p, Version: v}
			return nil
		}
		if v.State != "pending" && v.State != "uncertain" {
			return core.ErrConflict
		}
		if p.RemoteID != "" && p.RemoteID != in.RemoteSkillID {
			return core.ErrConflict
		}
		if e = lockRemote(ctx, tx, p.PluginKey, p.Kind, p.Binding, in.RemoteSkillID); e != nil {
			return e
		}
		known, e := lookupRemote(ctx, tx, p.PluginKey, p.Kind, p.Binding, in.RemoteSkillID)
		if e != nil {
			return e
		}
		for _, other := range known {
			if other.PublicID != p.PublicID {
				return core.ErrConflict
			}
		}
		if len(in.ParentMetadata) == 0 {
			if p.RemoteID == "" {
				return core.ErrInvalidArgument
			}
			pm = p.Metadata
		}
		e = update(g, resourcesTable, p.PublicID, map[string]any{"remote_id": in.RemoteSkillID, "state": "ready", "metadata": pm}).Error
		if e != nil {
			return e
		}
		e = update(g, versionsTable, v.PublicVersionID, map[string]any{
			"remote_id": in.RemoteVersionID, "legacy_epoch": in.LegacyEpoch, "state": "ready", "metadata": vm, "created_at": in.CreatedAt,
		}).Error
		if store.IsUniqueViolation(e, "") {
			return core.ErrConflict
		}
		if e != nil {
			return e
		}
		p, _, e = read(g, `public_id=?`, p.PublicID)
		if e != nil {
			return e
		}
		v, e = readSkillVersion(g, p, `public_id=?`, v.PublicVersionID)
		out = core.SkillUploadReservation{Parent: p, Version: v}
		return e
	})
	return
}

func confirmedSkillRejection(code string) bool {
	return code == "invalid_request_error" || code == "authentication_error" || code == "permission_error"
}
func (s *Service) MarkSkillUpload(ctx context.Context, in core.SkillUploadFailure) error {
	if in.Outcome != "uncertain" && (in.Outcome != "rejected" || !confirmedSkillRejection(in.EvidenceCode)) {
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
		v, e := readSkillVersion(g, p, `parent_id=? AND public_id=?`, p.PublicID, in.PublicVersionID)
		if e != nil {
			return e
		}
		target := "uncertain"
		if in.Outcome == "rejected" {
			target = "failed"
		}
		if v.OperationID != in.OperationID || (v.State != "pending" && v.State != "uncertain" && v.State != target) {
			return core.ErrConflict
		}
		meta, e := mergeSkillUploadEvidence(v.Metadata, in.Evidence, p.Binding)
		if e != nil {
			return e
		}
		if e = update(g, versionsTable, v.PublicVersionID, map[string]any{"state": target, "metadata": meta}).Error; e != nil {
			return e
		}
		if p.RemoteID == "" {
			e = update(g, resourcesTable, p.PublicID, map[string]any{"state": target}).Error
		}
		return e
	})
}

func mergeSkillUploadEvidence(prior json.RawMessage, evidence *core.SkillUploadEvidence, binding core.ResourceBinding) (json.RawMessage, error) {
	if evidence == nil {
		return metadata(prior)
	}
	if evidence.Binding != binding || !validBinding(evidence.Binding) {
		return nil, core.ErrConflict
	}
	for _, value := range []string{evidence.RemoteSkillID, evidence.RemoteVersionID, evidence.LegacyEpoch, evidence.SourceRequestID} {
		if value != "" && !bounded(value) {
			return nil, core.ErrInvalidArgument
		}
	}
	var root map[string]json.RawMessage
	if len(prior) > 0 && json.Unmarshal(prior, &root) != nil {
		return nil, core.ErrInvalidArgument
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	var old core.SkillUploadEvidence
	if value := root["upload_evidence"]; len(value) > 0 && json.Unmarshal(value, &old) != nil {
		return nil, core.ErrConflict
	}
	if old.Binding != (core.ResourceBinding{}) && old.Binding != binding {
		return nil, core.ErrConflict
	}
	merged := *evidence
	for _, pair := range [][2]*string{{&merged.RemoteSkillID, &old.RemoteSkillID}, {&merged.RemoteVersionID, &old.RemoteVersionID}, {&merged.LegacyEpoch, &old.LegacyEpoch}, {&merged.SourceRequestID, &old.SourceRequestID}} {
		if *pair[0] == "" {
			*pair[0] = *pair[1]
		} else if *pair[1] != "" && *pair[0] != *pair[1] {
			return nil, core.ErrConflict
		}
	}
	raw, _ := json.Marshal(merged)
	root["upload_evidence"] = raw
	raw, _ = json.Marshal(root)
	return metadata(raw)
}
