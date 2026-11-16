package core

import (
	"context"
	"maps"
	"slices"

	"github.com/Sub2API-Devs/sup2api/next/sdk/hierarchy"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// Can asks az whether the caller (UserID) holds k; the request's access key
// narrows the answer as usual. A nil az or no caller is "no"; a check that
// cannot be decided returns az's error (503 unavailable), never "no".
func Can(ctx context.Context, az Authorizer, k perm.Key) (bool, error) {
	if az == nil {
		return false, nil
	}
	uid, ok := UserID(ctx)
	if !ok {
		return false, nil
	}
	return az.Can(ctx, uid, string(k))
}

type scopesCtxKey struct{}

// WithScope records the caller's range of action, as Router.PermScoped
// computed it for the request.
func WithScope(ctx context.Context, action perm.Key, s DataScope) context.Context {
	prev := routeScopes(ctx)
	next := make(map[perm.Key]DataScope, len(prev)+1)
	maps.Copy(next, prev)
	next[action] = s
	return context.WithValue(ctx, scopesCtxKey{}, next)
}

// routeScopes are the scopes WithScope recorded.
func routeScopes(ctx context.Context) map[perm.Key]DataScope {
	m, _ := ctx.Value(scopesCtxKey{}).(map[perm.Key]DataScope)
	return m
}

// Scope is the rows the caller (UserID) reaches with action in this
// request, its access key applied: the route's (Router.PermScoped) when it
// computed one, otherwise asked of az. No caller, or a nil az, reaches
// nothing.
func Scope(ctx context.Context, az Authorizer, action perm.Key) (DataScope, error) {
	if s, ok := routeScopes(ctx)[action]; ok {
		return s, nil
	}
	uid, ok := UserID(ctx)
	if !ok || az == nil {
		return DataScope{}, nil
	}
	set, err := az.PermissionSet(ctx, uid)
	if err != nil {
		return DataScope{}, err
	}
	return set.DataScope(uid, action), nil
}

// MaxTeamDepth is how many levels of owners a user may have above it: a
// team spans at most that many below its holder (CONTRACTS §103).
const MaxTeamDepth = 8

// UserTree is the users by owner (users.owner_id, with users.owner_path;
// sdk/hierarchy): a team scope reaches the rows owned by a user below its
// holder (ScopeTable), and authz moves users in it (OwnerGuard).
func UserTree() hierarchy.Tree {
	return hierarchy.Tree{Table: "users", ID: "id", Parent: "owner_id", Path: "owner_path", MaxDepth: MaxTeamDepth}
}

// ProxyRefScope is the proxies the caller may reference from an account
// written under accountAction (proxy_id, proxy_url; CONTRACTS §21.4): every
// proxy when it holds accountAction for every account, otherwise those of
// its proxy:read and proxy:manage scopes.
func ProxyRefScope(ctx context.Context, az Authorizer, accountAction perm.Key) (DataScope, error) {
	if s, err := Scope(ctx, az, accountAction); err != nil || s.All {
		return s, err
	}
	read, err := Scope(ctx, az, perm.ProxyRead)
	if err != nil {
		return DataScope{}, err
	}
	manage, err := Scope(ctx, az, perm.ProxyManage)
	return read.Union(manage), err
}

// HasGranted reports whether the route middleware matched k for the caller
// (Granted).
func HasGranted(ctx context.Context, k perm.Key) bool {
	return slices.Contains(Granted(ctx), string(k))
}
