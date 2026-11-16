package core

import (
	"cmp"
	"slices"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// DataScope is the rows one caller reaches with one action: the union of
// the scopes of its patterns (perm.Pattern), compiled for SQL and memory
// checks (CONTRACTS §21). The zero value reaches nothing. Group membership
// is never compiled in: a group range is checked against account_groups as
// it is now, so binding or unbinding an account takes effect at once.
type DataScope struct {
	All bool
	// Owners: rows owned by one of them (the owner column of ScopeTable),
	// sorted; own and team give the holder.
	Owners []int64
	// Teams: rows owned by a user below one of them (UserTree), sorted;
	// team gives the holder (CONTRACTS §102). The users below are read by
	// the statement itself, never listed here.
	Teams  []int64
	Groups []int64 // group/G; 0 = group/* (bound to any group)
	IDs    []int64 // account/X or proxy/X
	PairG  []int64 // group/G/account/X: G, 0 for group/*; PairID holds X
	PairID []int64
}

// AllScope reaches every row (a superuser, an unscoped grant).
var AllScope = DataScope{All: true}

// Compile is the DataScope of uid holding scopes, sorted and without
// duplicates, so equal inputs give equal scopes.
func Compile(uid int64, scopes []perm.Scope) DataScope {
	var s DataScope
	type pair struct{ g, x int64 }
	var pairs []pair
	for _, sc := range scopes {
		switch {
		case sc.All:
			return AllScope
		case sc.Own:
			s.Owners = []int64{uid}
		case sc.Team:
			s.Owners, s.Teams = []int64{uid}, []int64{uid}
		case sc.Res != "":
			// A plugin scope reaches no account or proxy row; the plugin
			// judges its own rows.
			continue
		case sc.Group != 0 && sc.ID != 0:
			pairs = append(pairs, pair{max(sc.Group, 0), sc.ID})
		case sc.Group != 0:
			s.Groups = append(s.Groups, max(sc.Group, 0))
		default:
			s.IDs = append(s.IDs, sc.ID)
		}
	}
	s.Groups = sortedSet(s.Groups)
	s.IDs = sortedSet(s.IDs)
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(a.g, b.g), cmp.Compare(a.x, b.x))
	})
	for _, p := range slices.Compact(pairs) {
		s.PairG = append(s.PairG, p.g)
		s.PairID = append(s.PairID, p.x)
	}
	return s
}

func sortedSet(ids []int64) []int64 {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Union is what s and o reach together (both of the same caller).
func (s DataScope) Union(o DataScope) DataScope {
	if s.All || o.All {
		return AllScope
	}
	type pair struct{ g, x int64 }
	u := DataScope{Owners: sortedSet(slices.Concat(s.Owners, o.Owners)), Teams: sortedSet(slices.Concat(s.Teams, o.Teams)),
		Groups: sortedSet(slices.Concat(s.Groups, o.Groups)), IDs: sortedSet(slices.Concat(s.IDs, o.IDs))}
	var pairs []pair
	for i := range s.PairID {
		pairs = append(pairs, pair{s.PairG[i], s.PairID[i]})
	}
	for i := range o.PairID {
		pairs = append(pairs, pair{o.PairG[i], o.PairID[i]})
	}
	slices.SortFunc(pairs, func(a, b pair) int { return cmp.Or(cmp.Compare(a.g, b.g), cmp.Compare(a.x, b.x)) })
	for _, p := range slices.Compact(pairs) {
		u.PairG, u.PairID = append(u.PairG, p.g), append(u.PairID, p.x)
	}
	return u
}

// Empty reports whether s reaches no row.
func (s DataScope) Empty() bool {
	return !s.All && len(s.Owners) == 0 && len(s.Groups) == 0 && len(s.IDs) == 0 && len(s.PairID) == 0
}

// Owned reports whether s reaches rows by their owner (own or team): a row
// the holder creates is in s.
func (s DataScope) Owned() bool { return len(s.Owners) > 0 }

// Restricted reports whether s is short of every row: the restricted tier,
// which keeps every limit of the former own level (administrator lock,
// guarded settings, private proxies, relay accounts…).
func (s DataScope) Restricted() bool { return !s.All }

// Grouped reports whether s has a part that depends on group membership.
func (s DataScope) Grouped() bool { return len(s.Groups) > 0 || len(s.PairID) > 0 }

// HasGroup reports whether s reaches every account bound to group g
// (group/g or group/*).
func (s DataScope) HasGroup(g int64) bool {
	return s.All || slices.Contains(s.Groups, 0) || slices.Contains(s.Groups, g)
}

// AllowsRow reports whether the row id, owned by owner (its owner column,
// ScopeTable) and bound to groups (its current account_groups; nil for
// proxies), is in s. It does not read the users below a team's holder
// (Teams), which only a statement does: here a team reaches what own does.
func (s DataScope) AllowsRow(id int64, owner *int64, groups []int64) bool {
	switch {
	case s.All, owner != nil && slices.Contains(s.Owners, *owner), slices.Contains(s.IDs, id):
		return true
	case len(groups) == 0:
		return false
	case slices.Contains(s.Groups, 0) || slices.ContainsFunc(groups, func(g int64) bool { return slices.Contains(s.Groups, g) }):
		return true
	}
	for i, x := range s.PairID {
		if x == id && (s.PairG[i] == 0 || slices.Contains(groups, s.PairG[i])) {
			return true
		}
	}
	return false
}

// ScopeTable is how a DataScope reads a table: its row id and owner
// columns, and whether its rows belong to groups (account_groups).
// Its columns are SQL text: unexported, set by AccountRows, ProxyRows and
// UserRows alone, so that no package can change the text of another's
// condition.
type ScopeTable struct {
	id, owner string
	grouped   bool
}

// AccountRows is accounts aliased a, owned by owner_id (所属, which an
// administrator may move away from created_by; CONTRACTS §102).
func AccountRows() ScopeTable { return ScopeTable{id: "a.id", owner: "a.owner_id", grouped: true} }

// ProxyRows is proxies aliased p, owned by their creator.
func ProxyRows() ScopeTable { return ScopeTable{id: "p.id", owner: "p.created_by"} }

// UserRows is users aliased u, owned by owner_id: a team scope (the only
// one of user actions besides every row) reaches the users below the
// holder (those it owns, Owners, and those they own, Teams), not the
// holder itself.
func UserRows() ScopeTable { return ScopeTable{id: "u.id", owner: "u.owner_id"} }

// cond is the predicate of t with ? placeholders, one per args value (see
// Scope). It is never NULL (a NULL owner is no match), so it also reads
// right under NOT or as a selected flag. Its text does not depend on the
// scope, so the plan of a query is reused whatever ids the scope holds.
func (t ScopeTable) cond() string {
	c := "(?::boolean OR COALESCE(" + t.owner + " = ANY(?::bigint[]) OR " + t.owner + " IN (?), false) OR " + t.id + " = ANY(?::bigint[])"
	if t.grouped {
		// group/* is a flag of its own so that the group ids reach
		// account_groups_group_idx (an "= 0 OR =" join would not).
		c += " OR EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = " + t.id + " AND (?::boolean OR ag.group_id = ANY(?::bigint[])))" +
			" OR EXISTS (SELECT 1 FROM account_groups ag JOIN unnest(?::bigint[], ?::bigint[]) sp(g, x) ON sp.x = ag.account_id AND (sp.g = 0 OR sp.g = ag.group_id) WHERE ag.account_id = " + t.id + ")"
	}
	return c + ")"
}
