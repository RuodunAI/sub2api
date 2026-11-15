package casbinx

import (
	"errors"
	"slices"
	"strings"

	"github.com/casbin/casbin/v3/rbac"
)

// ErrNotWarm is the error of a resolution for a user the RoleSource holds no
// entry for: the caller did not warm the user's roles, or they were evicted
// since. Grants fails rather than answer with an empty set, which would be
// cached as the user's permissions at that version.
var ErrNotWarm = errors.New("casbinx: the user's roles are not in the role cache")

// errReadOnly answers every write: links come from the RoleSource, and
// nothing changes them through Casbin.
var errReadOnly = errors.New("casbinx: the role manager is read-only")

// roleManager is a read-only rbac.RoleManager over a RoleSource: a user
// subject links to the roles the source names (SelfRole included for an
// active user, none for another); a role links to itself only, as there is
// no role hierarchy. It does no I/O: the caller warms the source first.
type roleManager struct{ src RoleSource }

var _ rbac.RoleManager = (*roleManager)(nil)

func newRoleManager(src RoleSource) *roleManager { return &roleManager{src: src} }

// roles is name's roles; ok false for a user subject the source holds no
// entry for. A role subject has none.
func (m *roleManager) roles(name string) (roles []string, ok bool) {
	if !strings.HasPrefix(name, "u:") {
		return nil, true
	}
	return m.src.Roles(name)
}

// HasLink is g in the matcher. A user the source does not hold links to
// nothing (fail closed); Casbin drops this error anyway.
func (m *roleManager) HasLink(name1, name2 string, _ ...string) (bool, error) {
	if name1 == name2 {
		return true, nil
	}
	roles, _ := m.roles(name1)
	return slices.Contains(roles, name2), nil
}

// GetRoles is the roles of name. For a user the source does not hold it
// fails with ErrNotWarm, which GetImplicitPermissionsForUser returns.
func (m *roleManager) GetRoles(name string, _ ...string) ([]string, error) {
	roles, ok := m.roles(name)
	if !ok {
		return nil, ErrNotWarm
	}
	return slices.Clone(roles), nil
}

// GetImplicitRoles is GetRoles: roles inherit nothing.
func (m *roleManager) GetImplicitRoles(name string, domain ...string) ([]string, error) {
	return m.GetRoles(name, domain...)
}

// Clear is called by BuildRoleLinks; the source is not the manager's to
// clear.
func (m *roleManager) Clear() error { return nil }

func (m *roleManager) AddLink(string, string, ...string) error           { return errReadOnly }
func (m *roleManager) BuildRelationship(string, string, ...string) error { return errReadOnly }
func (m *roleManager) DeleteLink(string, string, ...string) error        { return errReadOnly }
func (m *roleManager) DeleteDomain(string) error                         { return errReadOnly }

// GetUsers and GetImplicitUsers would need every user's roles: the source
// holds the warmed ones only, so any answer would be partial.
func (m *roleManager) GetUsers(string, ...string) ([]string, error) { return nil, errReadOnly }
func (m *roleManager) GetImplicitUsers(string, ...string) ([]string, error) {
	return nil, errReadOnly
}

func (m *roleManager) GetDomains(string) ([]string, error)             { return nil, nil }
func (m *roleManager) GetAllDomains() ([]string, error)                { return nil, nil }
func (m *roleManager) PrintRoles() error                               { return nil }
func (m *roleManager) Match(str, pattern string) bool                  { return str == pattern }
func (m *roleManager) AddMatchingFunc(string, rbac.MatchingFunc)       {}
func (m *roleManager) AddDomainMatchingFunc(string, rbac.MatchingFunc) {}
