package install

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/grants"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// ConsentRequest is the body of POST /plugins/:key/versions/:version/consent.
type ConsentRequest struct {
	Grants []GrantInput `json:"grants"`
	Denied []string     `json:"denied"`
	// NewPermissionRoles overrides, per local userPermission key, the grants
	// of a permission created now: role -> true (every row) or scope values
	// ("*", "own", an enum key of scope.values or its canonical
	// "value/<key>", a runtime id "flow/3"). A key present replaces the
	// defaults for the roles the actor may name: those it leaves out get
	// nothing, {} grants none; a default to a role the actor may not name
	// still goes to review (D2). Keys absent keep the manifest defaults;
	// inheritFrom applies either way.
	NewPermissionRoles map[string]map[string]manifest.RoleDefault `json:"new_permission_roles,omitempty"`
}

// GrantInput approves one host permission, optionally with a narrower scope.
type GrantInput struct {
	Permission string         `json:"permission"`
	Scope      map[string]any `json:"scope,omitempty"`
}

// ConsentResult is returned after consent.
type ConsentResult struct {
	PluginKey string  `json:"plugin_key"`
	Version   string  `json:"version"`
	Status    string  `json:"status"`
	Upgrade   bool    `json:"upgrade"`
	Grants    []Grant `json:"grants"`
	// Withheld are the role grants this consent left for review (D2).
	Withheld []RoleGrant `json:"withheld"`
}

type decision struct {
	permission string
	scope      map[string]any
	status     string
	carried    bool // kept from a previous consent without re-approval
}

// decide resolves the consent request against the requested permissions.
// Rules: low-risk permissions are granted automatically; optional ones not
// mentioned are denied; on upgrades a permission whose current grant already
// covers what is granted now is carried over, whether the request names it
// or not (it was approved before, so granting it again takes no grant
// right); every other required permission must be granted explicitly.
// A revoked permission (grants.Revoked) is granted only when the request
// names it: required, it must be; optional, it stays revoked, denied or not.
// Granted scopes may be narrower than requested, never wider.
func decide(m *manifest.Manifest, req ConsentRequest, current map[string]Grant, revoked map[string]bool, upgrade bool) ([]decision, []core.FieldError) {
	var errs []core.FieldError
	requested := map[string]manifest.HostPermission{}
	for _, hp := range m.HostPermissions {
		requested[hp.ID] = hp
	}
	grants := map[string]GrantInput{}
	for i, g := range req.Grants {
		f := fmt.Sprintf("grants[%d]", i)
		if _, ok := requested[g.Permission]; !ok {
			errs = append(errs, core.FieldError{Field: f, Code: "not_requested", Message: fmt.Sprintf("permission %q is not requested by the plugin", g.Permission)})
			continue
		}
		if _, dup := grants[g.Permission]; dup {
			errs = append(errs, core.FieldError{Field: f, Code: "duplicate", Message: fmt.Sprintf("permission %q listed twice", g.Permission)})
		}
		grants[g.Permission] = g
	}
	denied := map[string]bool{}
	for i, d := range req.Denied {
		f := fmt.Sprintf("denied[%d]", i)
		if _, ok := requested[d]; !ok {
			errs = append(errs, core.FieldError{Field: f, Code: "not_requested", Message: fmt.Sprintf("permission %q is not requested by the plugin", d)})
			continue
		}
		if _, both := grants[d]; both {
			errs = append(errs, core.FieldError{Field: f, Code: "conflict", Message: fmt.Sprintf("permission %q is both granted and denied", d)})
		}
		denied[d] = true
	}

	// carried reports whether the current grant of id covers scope.
	carried := func(id string, scope map[string]any) bool {
		cur, ok := current[id]
		return upgrade && ok && cur.Status == GrantGranted && pkg.ScopeWithin(scope, cur.Scope)
	}
	var out []decision
	for _, hp := range m.HostPermissions {
		reqScope := pkg.NormalizeScope(hp.Scope)
		risk := riskOf(hp.ID)
		f := "permission:" + hp.ID
		if g, ok := grants[hp.ID]; ok {
			scope := reqScope
			if g.Scope != nil {
				scope = pkg.NormalizeScope(g.Scope)
				if !pkg.ScopeWithin(scope, reqScope) {
					errs = append(errs, core.FieldError{Field: f, Code: "scope_too_wide", Message: fmt.Sprintf("granted scope of %q is wider than requested", hp.ID)})
					continue
				}
			}
			out = append(out, decision{permission: hp.ID, scope: scope, status: GrantGranted, carried: carried(hp.ID, scope)})
			continue
		}
		if denied[hp.ID] || revoked[hp.ID] {
			switch {
			case hp.Optional && revoked[hp.ID]:
				out = append(out, decision{permission: hp.ID, scope: reqScope, status: GrantRevoked})
			case hp.Optional:
				out = append(out, decision{permission: hp.ID, scope: reqScope, status: GrantDenied})
			case denied[hp.ID]:
				errs = append(errs, core.FieldError{Field: f, Code: "required", Message: fmt.Sprintf("permission %q is required and cannot be denied", hp.ID)})
			default:
				errs = append(errs, core.FieldError{Field: f, Code: "consent_required", Message: fmt.Sprintf("permission %q was revoked and must be granted explicitly", hp.ID)})
			}
			continue
		}
		if risk == manifest.RiskLow {
			out = append(out, decision{permission: hp.ID, scope: reqScope, status: GrantGranted})
			continue
		}
		if carried(hp.ID, reqScope) {
			out = append(out, decision{permission: hp.ID, scope: reqScope, status: GrantGranted, carried: true})
			continue
		}
		if hp.Optional {
			out = append(out, decision{permission: hp.ID, scope: reqScope, status: GrantDenied})
			continue
		}
		errs = append(errs, core.FieldError{Field: f, Code: "consent_required", Message: fmt.Sprintf("permission %q must be granted explicitly", hp.ID)})
	}
	return out, errs
}

// overrideGrants normalizes the consent's overrides to role -> canonical
// scope suffixes per local key ("" every row, which makes the rest
// redundant; a role given false or [] is left out). Field errors name
// new_permission_roles.<local> for an undeclared key (unknown) and
// new_permission_roles.<local>.<role> for a scope value the permission does
// not take (invalid, as the catalog's own check).
func overrideGrants(m *manifest.Manifest, in map[string]map[string]manifest.RoleDefault) (map[string]map[string][]string, []core.FieldError) {
	out := make(map[string]map[string][]string, len(in))
	var errs []core.FieldError
	for _, local := range slices.Sorted(maps.Keys(in)) {
		up, ok := m.FindUserPermission(local)
		if !ok {
			errs = append(errs, core.FieldError{Field: "new_permission_roles." + local, Code: "unknown", Message: fmt.Sprintf("permission %q is not declared by the plugin", local)})
			continue
		}
		key := manifest.PermissionKey(m.Key, local)
		roles := map[string][]string{}
		for _, role := range slices.Sorted(maps.Keys(in[local])) {
			d := in[local][role]
			var suffixes []string
			if d.Unscoped {
				suffixes = append(suffixes, "")
			}
			for _, v := range d.Scopes {
				sfx, ok := scopeSuffix(key, up.Scope, v)
				if !ok {
					errs = append(errs, core.FieldError{Field: "new_permission_roles." + local + "." + role, Code: "invalid",
						Message: fmt.Sprintf("permission %q does not take the scope %q", local, v)})
					continue
				}
				suffixes = append(suffixes, sfx)
			}
			switch {
			case len(suffixes) == 0:
				continue
			case slices.Contains(suffixes, ""):
				suffixes = []string{""}
			default:
				slices.Sort(suffixes)
				suffixes = slices.Compact(suffixes)
			}
			roles[role] = suffixes
		}
		out[local] = roles
	}
	return out, errs
}

// scopeSuffix is the canonical suffix of v, one scope value a consent names
// for the permission key of scope s: the manifest spelling ("*", "own", a
// values key: GrantSuffix) or a canonical suffix of the resource s ranges
// over ("value/input", "flow/3"), so the console may send either.
func scopeSuffix(key string, s *manifest.PermissionScope, v string) (string, bool) {
	if v == "*" {
		return "", true
	}
	if s == nil {
		return "", false
	}
	if sfx, ok := s.GrantSuffix(v); ok {
		return sfx, true
	}
	p, err := perm.Parse(key + ":" + v)
	switch {
	case err != nil || string(p.Action) != key:
		return "", false
	case p.Scope.Own:
		return p.Suffix(), s.Own
	case p.Scope.Res != s.ResourceName():
		return "", false
	case len(s.Values) > 0:
		return p.Suffix(), slices.ContainsFunc(s.Values, func(sv manifest.ScopeValue) bool { return sv.Key == p.Scope.Value })
	}
	return p.Suffix(), true
}

// consentable is the locked state Consent decides on.
type consentable struct {
	pluginStatus  string
	activeVersion *string
	publisherID   *int64
	manifestHash  string
	trust         string
	manifest      *manifest.Manifest
	upgrade       bool
	current       map[string]Grant
	revoked       map[string]bool
	staging       bool
}

// staged reports that the decisions wait for the version's activation:
// another version runs (or an enable is preparing it), and it keeps running
// with its own grants.
func (c *consentable) staged() bool { return c.staging }

// grantDecisions are the decisions as grant rows.
func grantDecisions(ds []decision) []grants.Decision {
	out := make([]grants.Decision, len(ds))
	for i, d := range ds {
		scope, _ := json.Marshal(d.scope)
		out[i] = grants.Decision{Permission: d.permission, Scope: scope, Status: d.status}
	}
	return out
}

// loadConsentable locks the plugin and version rows and re-checkes every
// barrier an approval must pass: uninstall in progress, consent status,
// signature revocation, publisher revocation and exclusive resources.
func (s *Service) loadConsentable(ctx context.Context, tx pgx.Tx, key, version string) (*consentable, error) {
	c := &consentable{}
	// live is the version instances run or prepare: the active one, else
	// the one an enable rollout is preparing.
	var live *string
	err := tx.QueryRow(ctx, `SELECT status, active_version, COALESCE(active_version, desired_version), publisher_id FROM plugins WHERE key = $1 FOR UPDATE`, key).
		Scan(&c.pluginStatus, &c.activeVersion, &live, &c.publisherID)
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound.WithMessage("plugin not found")
	}
	if err != nil {
		return nil, err
	}
	var consent, sigStatus string
	if err := checkUninstall(ctx, tx, key); err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT consent_status, signature_status, manifest_hash FROM plugin_versions
		WHERE plugin_key = $1 AND version = $2 FOR UPDATE`, key, version).Scan(&consent, &sigStatus, &c.manifestHash)
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound.WithMessage("plugin version not found")
	}
	if err != nil {
		return nil, err
	}
	if consent != ConsentAwaiting {
		return nil, core.ErrConflict.WithMessage(fmt.Sprintf("version %s is %s", version, consent))
	}
	if sigStatus == pkg.SigRevoked {
		return nil, core.ErrPermissionDenied.WithMessage("the package signature has been revoked")
	}
	c.trust = pkg.TrustUnsigned
	if c.publisherID != nil {
		var pubStatus string
		if err := tx.QueryRow(ctx, `SELECT trust_level, status FROM publishers WHERE id = $1`, *c.publisherID).Scan(&c.trust, &pubStatus); err != nil {
			return nil, err
		}
		if pubStatus != "active" {
			return nil, core.ErrPermissionDenied.WithMessage("the publisher has been revoked")
		}
	} else if !s.d.Trust.AllowUnsigned() {
		return nil, core.ErrPermissionDenied.WithMessage("unsigned plugins are not allowed")
	}
	m, err := LoadManifest(ctx, tx, key, version)
	if err != nil {
		return nil, err
	}
	c.manifest = m
	if err := pkg.CheckTrust(m, c.trust); err != nil {
		return nil, err
	}
	// Exclusive resources are only held once approved: another plugin may
	// have been approved for them since this version was uploaded.
	if err := s.checkResources(ctx, tx, m); err != nil {
		return nil, err
	}
	c.upgrade = c.pluginStatus != StatusAwaitingConsent
	c.staging = live != nil && *live != version
	c.current = map[string]Grant{}
	if c.upgrade {
		if c.current, err = LoadGrants(ctx, tx, key); err != nil {
			return nil, err
		}
	}
	if c.revoked, err = grants.Revoked(ctx, tx, key); err != nil {
		return nil, err
	}
	return c, nil
}

// decideAndApply records the operator's decisions and moves the plugin into
// its post-consent state.
func (s *Service) decideAndApply(ctx context.Context, tx pgx.Tx, key, version string, req ConsentRequest, actorID int64, c *consentable, auto bool) (*ConsentResult, error) {
	m := c.manifest
	decisions, ferrs := decide(m, req, c.current, c.revoked, c.upgrade)
	overrides, oerrs := overrideGrants(m, req.NewPermissionRoles)
	if ferrs = append(ferrs, oerrs...); len(ferrs) > 0 {
		return nil, core.InvalidFields(ferrs...).WithMessage("consent does not match the requested permissions")
	}
	if err := s.checkGrantRights(ctx, actorID, decisions, c.trust, c.publisherID != nil); err != nil {
		return nil, err
	}
	existing, _, err := catalogKeys(ctx, tx, m)
	if err != nil {
		return nil, err
	}
	if err := s.checkRoleGrantRights(ctx, tx, actorID, m, req, overrides, existing); err != nil {
		return nil, err
	}
	st, err := s.settleRoleGrants(ctx, tx, actorID, c, existing, roleGrants(m, overrides, existing), overrides, auto)
	if err != nil {
		return nil, err
	}
	if err := grants.Record(ctx, tx, key, version, c.manifestHash, nullID(actorID), c.staged(), grantDecisions(decisions)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE plugin_versions SET consent_status = 'approved' WHERE plugin_key = $1 AND version = $2`, key, version); err != nil {
		return nil, err
	}

	status := c.pluginStatus
	switch {
	case !c.upgrade:
		if err := s.d.Defaults.ApplyDefaults(ctx, tx, m, st.roles); err != nil {
			return nil, err
		}
		name, _ := json.Marshal(m.Name)
		if _, err := tx.Exec(ctx, `UPDATE plugins SET status = $2, name = $3, installed_by = COALESCE(installed_by, $4),
				installed_at = now(), updated_at = now(), row_version = row_version + 1 WHERE key = $1`,
			key, StatusInstalled, name, nullID(actorID)); err != nil {
			return nil, err
		}
		status = StatusInstalled
	case c.activeVersion == nil:
		// Nothing is running: the newest approved version becomes the
		// one to enable, so its defaults apply now.
		if err := s.d.Defaults.ApplyDefaults(ctx, tx, m, st.roles); err != nil {
			return nil, err
		}
	default:
		// Register the new version's permissions next to the running
		// ones so role grants chosen now exist before activation; the
		// runtime's ApplyDefaults at activation removes stale ones.
		if s.d.Permissions != nil {
			old, err := LoadManifest(ctx, tx, key, *c.activeVersion)
			if err != nil {
				return nil, err
			}
			defs := unionDefs(PermissionDefs(old, nil), PermissionDefs(m, st.roles))
			if err := s.d.Permissions.SyncPlugin(ctx, tx, key, defs); err != nil {
				return nil, err
			}
		}
	}

	granted, err := LoadVersionGrants(ctx, tx, key, version)
	if err != nil {
		return nil, err
	}
	res := &ConsentResult{PluginKey: key, Version: version, Status: status, Upgrade: c.upgrade, Grants: SortedGrants(granted), Withheld: st.pending}
	summary := make([]map[string]any, 0, len(decisions))
	for _, d := range decisions {
		summary = append(summary, map[string]any{"permission": d.permission, "status": d.status, "scope": d.scope, "carried": d.carried})
	}
	detail := map[string]any{"version": version, "upgrade": c.upgrade, "decisions": summary, "new_permission_roles": req.NewPermissionRoles}
	if auto || isSystem(ctx) {
		// A consent nobody gave: its role grants are audited apart, where
		// the review queue reads them too.
		if err := auditAutoGrants(ctx, tx, actorID, key, version, st); err != nil {
			return nil, err
		}
	} else {
		detail["role_grants"], detail["pending"], detail["inherit_skipped"] = st.plan, st.pending, st.skipped
	}
	return res, audit.Audit(ctx, tx, actorID, AuditConsent, "plugin", key, detail)
}

// Consent approves a version. First installs apply defaults and move the
// plugin to installed; upgrades only approve the version (the runtime
// applies defaults when it activates).
func (s *Service) Consent(ctx context.Context, key, version string, req ConsentRequest, actorID int64) (*ConsentResult, error) {
	return s.consent(ctx, key, version, req, actorID, false)
}

// settlement is how a consent's role grants are settled: the grants to
// sync with, the plan, the grants withheld for review and the inheritFrom
// sources left out.
type settlement struct {
	roles   map[string]map[string][]string
	plan    []RoleGrant
	pending []RoleGrant
	skipped []InheritSkip
}

// settleRoleGrants plans the role grants of the consent (planRoleGrants),
// marks what the actor may name, decides what waits for review and checks
// the rest:
//   - system (a built-in plugin, isSystem): everything is nameable, the
//     grants that need consent are withheld;
//   - auto (an upload carried over): any grant that needs consent fails
//     with errRoleGrantsNeedConsent, else the unnameable ones are withheld;
//   - operator (the consent page): unnameable default and inherited grants
//     are withheld (D2); overrides were checked already (403).
//
// On an overridden key a default to a nameable role the override left out
// is dropped (the actor decided), to an unnameable one withheld.
func (s *Service) settleRoleGrants(ctx context.Context, tx pgx.Tx, actorID int64, c *consentable,
	existing map[string]bool, roles, overrides map[string]map[string][]string, auto bool,
) (*settlement, error) {
	m := c.manifest
	plan, skipped, err := planRoleGrants(ctx, tx, m, existing, roles, overrides, c.upgrade)
	if err != nil {
		return nil, err
	}
	system := isSystem(ctx)
	if auto && !system && needConsent(plan) {
		return nil, errRoleGrantsNeedConsent
	}
	gs, canDefine, err := s.grantees(ctx, tx, actorID, rolesOf(plan))
	if err != nil {
		return nil, err
	}
	for i := range plan {
		g := &plan[i]
		g.Nameable, _ = nameable(gs, canDefine, g.Role, g.Risk)
		g.Withheld = system && g.NeedsConsent || !system && g.Via != viaOverride && !g.Nameable
	}
	plan = slices.DeleteFunc(plan, func(g RoleGrant) bool {
		local, _ := manifest.LocalPermission(m.Key, g.Permission)
		return g.Via == viaDefault && g.Nameable && overrides[local] != nil
	})
	if !system {
		made := slices.DeleteFunc(slices.Clone(plan), func(g RoleGrant) bool { return g.Withheld || g.Via == viaOverride })
		if err := s.checkGrantable(ctx, tx, actorID, made); err != nil {
			return nil, err
		}
	}
	st := &settlement{plan: plan, skipped: skipped}
	st.roles, st.pending = withholdGrants(m, existing, plan)
	if st.plan == nil {
		st.plan = []RoleGrant{}
	}
	if st.skipped == nil {
		st.skipped = []InheritSkip{}
	}
	return st, nil
}

// nameable is the rule of whether the consenting actor may name role for a
// new permission of risk (D2, D3), and why not: role:define, critical only
// by a superuser, and the catalog's pluginGrantee and editableRole
// (PluginGrantees).
func nameable(gs core.PluginGrantees, canDefine bool, role, risk string) (bool, string) {
	switch {
	case !canDefine:
		return false, "role_define"
	case risk == manifest.RiskCritical && !gs.Superuser:
		return false, "critical"
	}
	if reason, denied := gs.Denied[role]; denied {
		return false, reason
	}
	return true, ""
}

// grantees reads what nameable needs of actorID for roles: its
// PluginGrantees and whether it has role:define. The system (isSystem, or
// actor 0) may name every role.
func (s *Service) grantees(ctx context.Context, tx pgx.Tx, actorID int64, roles []string) (core.PluginGrantees, bool, error) {
	if isSystem(ctx) || actorID == 0 {
		return core.PluginGrantees{Superuser: true}, true, nil
	}
	canDefine, err := s.d.Authz.Can(ctx, actorID, "role:define")
	if err != nil || !canDefine {
		return core.PluginGrantees{}, false, err
	}
	if s.d.Permissions == nil {
		return core.PluginGrantees{}, false, core.ErrInternal.WithMessage("the permission catalog is not available")
	}
	gs, err := s.d.Permissions.PluginGrantees(ctx, tx, actorID, roles)
	return gs, true, err
}

func (s *Service) consent(ctx context.Context, key, version string, req ConsentRequest, actorID int64, auto bool) (*ConsentResult, error) {
	ctx, release, guardErr := core.BeginPluginMutation(ctx, s.d.Mutations)
	if guardErr != nil {
		return nil, guardErr
	}
	defer release()
	if err := s.checkPackage(ctx, key, version); err != nil {
		return nil, err
	}

	var res *ConsentResult
	err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		c, err := s.loadConsentable(ctx, tx, key, version)
		if err != nil {
			return err
		}
		res, err = s.decideAndApply(ctx, tx, key, version, req, actorID, c, auto)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Notify(ctx, key)
	return res, nil
}

// dbSchemaUnconfined reports that granting db.schema takes
// plugin:grant:db_schema_unconfined: schemas are managed, without role
// isolation, for a plugin not from an official publisher. The consent check
// and the review (Review.NeedsDBSchemaUnconfined) both ask it.
func (s *Service) dbSchemaUnconfined(official bool) bool {
	return s.d.Schemas != nil && !s.opt.Plugins.DBRoleIsolation && !official
}

// checkGrantRights verifies the operator may approve every newly granted
// high/critical permission (carried-over grants were approved before).
// For db.schema: without role isolation, non-official plugins are rejected
// unless the operator explicitly has plugin:grant:db_schema_unconfined.
func (s *Service) checkGrantRights(ctx context.Context, actorID int64, ds []decision, trust string, hasPub bool) error {
	// Role isolation cannot be provided (no CREATEROLE): granting db.schema
	// is refused for every plugin, built-in ones included, instead of
	// running it with the core's database role (audit 2026-10-09 P1-7).
	for _, d := range ds {
		if d.permission == "db.schema" && d.status == GrantGranted {
			if c, ok := s.d.Schemas.(interface{ CheckIsolation(context.Context) error }); ok {
				if err := c.CheckIsolation(ctx); err != nil {
					return err
				}
			}
			break
		}
	}
	if isSystem(ctx) {
		return nil // built-in plugins ship with the image
	}
	need := map[string][]string{}
	var needsDBSchema bool
	for _, d := range ds {
		if d.status != GrantGranted || d.carried {
			continue
		}
		if d.permission == "db.schema" {
			needsDBSchema = true
		}
		if p := RequiredGrantPermission(riskOf(d.permission)); p != "" {
			need[p] = append(need[p], d.permission)
		}
	}

	// PL-P0-5: db.schema requires role isolation OR official trust OR explicit override
	if needsDBSchema {
		if s.dbSchemaUnconfined(hasPub && trust == pkg.TrustOfficial) {
			// Require explicit override permission
			ok, err := s.d.Authz.Can(ctx, actorID, "plugin:grant:db_schema_unconfined")
			if err != nil {
				return err
			}
			if !ok {
				return core.ErrPermissionDenied.WithMessage("granting db.schema to non-official plugins without role isolation requires plugin:grant:db_schema_unconfined")
			}
		}
	}

	perms := make([]string, 0, len(need))
	for p := range need {
		perms = append(perms, p)
	}
	sort.Strings(perms)
	for _, p := range perms {
		ok, err := s.d.Authz.Can(ctx, actorID, p)
		if err != nil {
			return err
		}
		if !ok {
			return core.ErrPermissionDenied.WithMessage(fmt.Sprintf("granting %v requires %s", need[p], p)).
				WithDetails(map[string]any{"permission": p, "host_permissions": need[p]})
		}
	}
	return nil
}

// checkRoleGrantRights verifies the operator may hand the permissions this
// version adds to the roles the request names (SEC-H2): role:define, and
// every such role must be one the operator may change (CanGrantToRoles:
// within the operator's own rights, on its assignable list and not held by
// it unless it is the super admin, never a super admin role; built-in roles
// alike; a critical permission by a superuser only, D3). The new
// permissions themselves need not be held. Defaults and inheritance are
// settled apart (settleRoleGrants: withheld when unnameable). Only
// permissions the catalog does not have yet are handed out (authz
// SyncPlugin), so an upgrade adding none needs nothing even with roles
// named - carrying a running plugin forward must not require role:define.
func (s *Service) checkRoleGrantRights(ctx context.Context, tx pgx.Tx, actorID int64, m *manifest.Manifest, req ConsentRequest,
	overrides map[string]map[string][]string, existing map[string]bool,
) error {
	if err := checkNamedRolesExist(ctx, tx, req.NewPermissionRoles); err != nil {
		return err
	}
	var named []RoleGrant
	for _, up := range m.UserPermissions {
		key := manifest.PermissionKey(m.Key, up.Key)
		if existing[key] {
			continue
		}
		for role := range overrides[up.Key] {
			named = append(named, RoleGrant{Permission: key, Role: role, Risk: up.RiskLevel(), Via: viaOverride})
		}
	}
	return s.checkGrantable(ctx, tx, actorID, named)
}

// checkGrantable verifies, with the catalog's locking CanGrantToRoles
// (role:define first), that the operator may make the grants of gs: those
// of critical permissions in a batch of their own, whose refusal also names
// their local keys (details.permissions).
func (s *Service) checkGrantable(ctx context.Context, tx pgx.Tx, actorID int64, gs []RoleGrant) error {
	var roles, critical, criticalKeys []string
	for _, g := range gs {
		if g.Risk != manifest.RiskCritical {
			roles = append(roles, g.Role)
			continue
		}
		critical = append(critical, g.Role)
		_, local, _ := strings.Cut(g.Permission, ":")
		criticalKeys = append(criticalKeys, local)
	}
	if err := s.checkRolesGrantable(ctx, tx, actorID, roles, false); err != nil {
		return err
	}
	err := s.checkRolesGrantable(ctx, tx, actorID, critical, true)
	if ce := core.AsError(err); ce != nil && ce.Details["reason"] == "critical" {
		slices.Sort(criticalKeys)
		return ce.WithDetails(map[string]any{"permissions": slices.Compact(criticalKeys)})
	}
	return err
}

// checkRolesGrantable verifies the operator may hand plugin permissions
// (critical ones when critical) to roles it names (none needs nothing):
// role:define and CanGrantToRoles.
func (s *Service) checkRolesGrantable(ctx context.Context, tx pgx.Tx, actorID int64, roles []string, critical bool) error {
	if isSystem(ctx) || len(roles) == 0 {
		return nil
	}
	ok, err := s.d.Authz.Can(ctx, actorID, "role:define")
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrPermissionDenied.WithMessage("granting permissions to roles requires role:define").
			WithDetails(map[string]any{"permission": "role:define"})
	}
	if s.d.Permissions == nil {
		return core.ErrInternal.WithMessage("the permission catalog is not available")
	}
	roles = slices.Clone(roles)
	slices.Sort(roles)
	return s.d.Permissions.CanGrantToRoles(ctx, tx, actorID, slices.Compact(roles), critical)
}

// checkNamedRolesExist fails (400) when the request names a role that does
// not exist, under new_permission_roles.<local>, whether or not the
// permission it is named for is created now.
func checkNamedRolesExist(ctx context.Context, q store.Querier, named map[string]map[string]manifest.RoleDefault) error {
	var roles []string
	for _, rs := range named {
		roles = append(roles, slices.Collect(maps.Keys(rs))...)
	}
	if len(roles) == 0 {
		return nil
	}
	var found []string
	if err := q.QueryRow(ctx, `SELECT COALESCE(array_agg(key), '{}') FROM roles WHERE key = ANY($1)`, roles).Scan(&found); err != nil {
		return err
	}
	var errs []core.FieldError
	for _, local := range slices.Sorted(maps.Keys(named)) {
		for _, r := range slices.Sorted(maps.Keys(named[local])) {
			if !slices.Contains(found, r) {
				errs = append(errs, core.FieldError{Field: "new_permission_roles." + local, Code: "unknown", Message: fmt.Sprintf("role %q does not exist", r)})
			}
		}
	}
	if len(errs) > 0 {
		return core.InvalidFields(errs...).WithMessage("the consent names unknown roles")
	}
	return nil
}

// catalogKeys are the keys of m's userPermissions and of their inheritFrom
// that the permission catalog has now, removed rows included (a key
// declared again is not new, D6), and of those the ones now removed
// (revived by this version). In consent, tx holds the plugin row, so no
// other consent changes them meanwhile.
func catalogKeys(ctx context.Context, q store.Querier, m *manifest.Manifest) (existing, revived map[string]bool, err error) {
	var keys []string
	for _, up := range m.UserPermissions {
		keys = append(keys, manifest.PermissionKey(m.Key, up.Key))
		for _, from := range up.InheritFrom {
			keys = append(keys, manifest.PermissionKey(m.Key, from))
		}
	}
	rows, err := q.Query(ctx, `SELECT key, status = 'removed' FROM permissions WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	existing, revived = map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var key string
		var removed bool
		if err := rows.Scan(&key, &removed); err != nil {
			return nil, nil, err
		}
		existing[key], revived[key] = true, removed
	}
	return existing, revived, rows.Err()
}

// Reject marks a version rejected. A plugin that was never installed and
// has no other pending version is removed.
func (s *Service) Reject(ctx context.Context, key, version string, actorID int64) error {
	ctx, release, guardErr := core.BeginPluginMutation(ctx, s.d.Mutations)
	if guardErr != nil {
		return guardErr
	}
	defer release()

	return s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM plugins WHERE key = $1 FOR UPDATE`, key).Scan(&status)
		if store.IsNoRows(err) {
			return core.ErrNotFound.WithMessage("plugin not found")
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE plugin_versions SET consent_status = 'rejected'
			WHERE plugin_key = $1 AND version = $2 AND consent_status = 'awaiting_consent'`, key, version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrConflict.WithMessage("version is not awaiting consent")
		}
		removed := false
		if status == StatusAwaitingConsent {
			var pending int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_versions WHERE plugin_key = $1 AND consent_status <> 'rejected'`, key).Scan(&pending); err != nil {
				return err
			}
			if pending == 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM plugins WHERE key = $1`, key); err != nil {
					return err
				}
				removed = true
			}
		}
		return audit.Audit(ctx, tx, actorID, "plugin.reject", "plugin", key, map[string]any{"version": version, "plugin_removed": removed})
	})
}

// RevokeGrant marks one host permission grant revoked, staged ones included.
func (s *Service) RevokeGrant(ctx context.Context, key, permission string, actorID int64) error {
	err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		// The plugin row first, as consent and the rollout commit lock it
		// before the grant rows they write (no lock-order deadlock).
		if _, err := tx.Exec(ctx, `UPDATE plugins SET row_version=row_version+1,updated_at=now() WHERE key=$1`, key); err != nil {
			return err
		}
		revoked, err := grants.Revoke(ctx, tx, key, permission, nullID(actorID))
		if err != nil {
			return err
		}
		if !revoked {
			return core.ErrNotFound.WithMessage("grant not found")
		}
		return audit.Audit(ctx, tx, actorID, "plugin.grant.revoke", "plugin", key, map[string]any{"permission": permission})
	})
	if err == nil {
		s.Notify(ctx, key)
	}
	return err
}
