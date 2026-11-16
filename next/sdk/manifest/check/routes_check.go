package check

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

func (v *validator) routes() {
	if len(v.m.Routes) == 0 {
		return
	}
	v.needCap("routes", manifest.CapHTTPRoutes)
	seen := map[string]bool{}
	for i, r := range v.m.Routes {
		f := fmt.Sprintf("routes[%d]", i)
		method := strings.ToUpper(r.Method)
		if !allowedMethods[method] {
			v.add(f+".method", "invalid", "unsupported method %q", r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") {
			v.add(f+".path", "invalid_path", "path must start with /")
		}
		sig := manifest.RouteSignature(r.Method, r.Path)
		if seen[sig] {
			v.add(f+".path", "duplicate", "route %s %s declared twice", method, r.Path)
		}
		seen[sig] = true
		switch r.Scope {
		case "admin", "user":
			if r.Permission == "" {
				v.add(f+".permission", "required", "permission is required for %s routes", r.Scope)
			} else if !v.userPerm(r.Permission) {
				v.add(f+".permission", "unknown", "permission %q is not declared in userPermissions", r.Permission)
			}
			v.routeAccounts(f+".accounts", r)
			v.routeScopeFrom(f+".scopeFrom", r)
		case "public", "webhook":
			// The proxy forwards these unauthenticated (W5-C6).
			if r.Permission != "" {
				v.add(f+".permission", "unsupported", noCallerMsg)
			}
			if r.Accounts != nil {
				v.add(f+".accounts", "unsupported", noCallerMsg)
			}
			if r.ScopeFrom != "" {
				v.add(f+".scopeFrom", "unsupported", noCallerMsg)
			}
		default:
			v.add(f+".scope", "invalid", "scope must be admin, user, public or webhook")
			continue
		}
		v.needPerm(f+".scope", "routes."+r.Scope, r.Scope+" routes")
	}
}

const noCallerMsg = "public/webhook routes carry no caller; permission is not enforced"

// routeAccounts validates routes[i].accounts of an admin or user route: a
// known action, and From entries naming a path parameter of the route, a
// query parameter or a body field.
func (v *validator) routeAccounts(f string, r manifest.Route) {
	if r.Accounts == nil {
		return
	}
	if !manifest.ValidAccountsAction(r.Accounts.Action) {
		v.add(f+".action", "invalid", "action must be read, update or test")
	}
	for i, from := range r.Accounts.From {
		v.valueFrom(fmt.Sprintf("%s.from[%d]", f, i), r, from)
	}
}

// routeScopeFrom validates routes[i].scopeFrom of an admin or user route:
// the route's permission declares a scope the named value is checked
// against. A route of a scoped permission other than GET must name the rows
// it acts on: any scope admits the route, so without scopeFrom only the
// plugin would keep the holder of one row from acting on every row. A
// route that names none (creating a row, say) takes an unscoped permission.
func (v *validator) routeScopeFrom(f string, r manifest.Route) {
	scoped := v.m.UserPermissionScope(r.Permission) != nil
	switch {
	case r.ScopeFrom != "" && !scoped:
		v.add(f, "unsupported", "permission %q declares no scope", r.Permission)
	case r.ScopeFrom != "":
		v.valueFrom(f, r, r.ScopeFrom)
	case scoped && strings.ToUpper(r.Method) != "GET":
		v.add(f, "required", "%s routes of the scoped permission %q name the rows they act on; give a route naming none an unscoped permission", strings.ToUpper(r.Method), r.Permission)
	}
}

// valueFrom checks one "path:|query:|body:" entry of route r (accounts.from,
// scopeFrom): a known source and plain name, a path parameter of r.
func (v *validator) valueFrom(f string, r manifest.Route, from string) {
	source, name, ok := manifest.ParseAccountsFrom(from)
	params := strings.Split(r.Path, "/")
	switch {
	case !ok:
		v.add(f, "invalid", "%q is not path:<param>, query:<name> or body:<field>", from)
	case source == manifest.AccountsFromPath && !slices.Contains(params, ":"+name) && !slices.Contains(params, "*"+name):
		v.add(f, "unknown_param", "route path %s has no parameter %q", r.Path, name)
	}
}
