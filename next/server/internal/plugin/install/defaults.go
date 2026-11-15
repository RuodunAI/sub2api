package install

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// DefaultsApplier implements core.PluginDefaultsApplier. It has no
// dependency on the rollout controller so the runtime (C2) can be built
// with it before the install service exists.
type DefaultsApplier struct {
	perms core.PermissionCatalog
}

var _ core.PluginDefaultsApplier = (*DefaultsApplier)(nil)

// NewDefaultsApplier wires the permission catalog. A nil catalog is skipped.
func NewDefaultsApplier(perms core.PermissionCatalog) *DefaultsApplier {
	return &DefaultsApplier{perms: perms}
}

// ApplyDefaults syncs the user permissions of m, granting newly created ones
// what grants holds for their local key (roleGrants, settled by the
// consent), and drops host permission grants the manifest no longer
// requests, except revocations: a version asking for the permission again
// does not get it by default (grants.Revoked). It runs inside the caller's
// transaction.
// Plugins bring no sticky rules (CONTRACTS §5.6).
func (a *DefaultsApplier) ApplyDefaults(ctx context.Context, tx pgx.Tx, m *manifest.Manifest, grants map[string]map[string][]string) error {
	if a.perms != nil {
		if err := a.perms.SyncPlugin(ctx, tx, m.Key, PermissionDefs(m, grants)); err != nil {
			return fmt.Errorf("sync plugin permissions: %w", err)
		}
	}
	ids := make([]string, 0, len(m.HostPermissions))
	for _, hp := range m.HostPermissions {
		ids = append(ids, hp.ID)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM plugin_permission_grants WHERE plugin_key = $1 AND NOT (permission = ANY($2)) AND status <> 'revoked'`, m.Key, ids); err != nil {
		return fmt.Errorf("prune grants: %w", err)
	}
	return nil
}

// PermissionDefs converts manifest userPermissions into catalog entries,
// sorted in manifest order (10, 20, ...), their groups likewise. A
// permission created now is granted what grants holds for its local key,
// role -> scope suffixes (nothing when absent): the consent resolved its
// defaults, overrides and inheritance into it. A nil grants (activation: no
// consent is behind a permission created then, one deleted since the
// consent) grants nothing, so a role never gets a permission nobody
// reviewed (P4C).
func PermissionDefs(m *manifest.Manifest, grants map[string]map[string][]string) []core.PermissionDef {
	defs := make([]core.PermissionDef, 0, len(m.UserPermissions))
	for i, up := range m.UserPermissions {
		d := core.PermissionDef{
			Key:         manifest.PermissionKey(m.Key, up.Key),
			Module:      manifest.PermissionModule(m.Key),
			Label:       core.LocalizedText(up.Label),
			Description: core.LocalizedText(up.Description),
			Risk:        up.RiskLevel(),
			Sort:        (i + 1) * 10,
			Group:       up.Group,
			Defaults:    up.DefaultGrants(),
			GrantRoles:  grants[up.Key],
		}
		if g := slices.IndexFunc(m.PermissionGroups, func(pg manifest.PermissionGroup) bool { return pg.Key == up.Group }); g >= 0 {
			d.GroupLabel, d.GroupSort = core.LocalizedText(m.PermissionGroups[g].Label), (g+1)*10
		}
		for _, r := range up.Requires {
			d.Requires = append(d.Requires, manifest.PermissionKey(m.Key, r))
		}
		if d.Scope = catalogScope(up.Scope); d.Scope != nil {
			d.ScopeType = core.PluginScopeTypePrefix + d.Scope.Resource
		}
		defs = append(defs, d)
	}
	return defs
}

// catalogScope is a manifest scope as the catalog and the review show it:
// Resource set to ResourceName() (the values resource for a values scope);
// nil for nil.
func catalogScope(s *manifest.PermissionScope) *manifest.PermissionScope {
	if s == nil {
		return nil
	}
	out := *s
	out.Resource = s.ResourceName()
	return &out
}

// unionDefs merges b into a (b wins on key collisions), preserving order.
func unionDefs(a, b []core.PermissionDef) []core.PermissionDef {
	idx := map[string]int{}
	out := make([]core.PermissionDef, 0, len(a)+len(b))
	for _, d := range a {
		idx[d.Key] = len(out)
		out = append(out, d)
	}
	for _, d := range b {
		if i, ok := idx[d.Key]; ok {
			out[i] = d
			continue
		}
		idx[d.Key] = len(out)
		out = append(out, d)
	}
	return out
}

// roleGrants resolves, per local userPermission key, the role -> scope
// suffixes a permission created now is planned with: nothing for a key the
// catalog has (existing, catalogKeys: removed rows included, so a key
// declared again gets no defaults, D6); else the manifest defaults
// (DefaultGrants), none for a rename or split, which inherits from an old
// key the catalog has and so only keeps that key's holders
// (planRoleGrants); the roles the consent's override names replace theirs
// (overrideGrants). Defaults to roles the override leaves out stay: the
// consent's settlement drops or withholds them (settleRoleGrants).
func roleGrants(m *manifest.Manifest, overrides map[string]map[string][]string, existing map[string]bool) map[string]map[string][]string {
	out := make(map[string]map[string][]string, len(m.UserPermissions))
	for _, up := range m.UserPermissions {
		roles := map[string][]string{}
		out[up.Key] = roles
		if existing[manifest.PermissionKey(m.Key, up.Key)] {
			continue
		}
		if !renamed(m, up, existing) {
			maps.Copy(roles, up.DefaultGrants())
		}
		maps.Copy(roles, overrides[up.Key])
	}
	return out
}

// renamed reports that up inherits from a key the catalog has (existing):
// a rename or split, which keeps the old key's holders instead of getting
// the manifest defaults.
func renamed(m *manifest.Manifest, up manifest.UserPermission, existing map[string]bool) bool {
	return slices.ContainsFunc(up.InheritFrom, func(from string) bool { return existing[manifest.PermissionKey(m.Key, from)] })
}
