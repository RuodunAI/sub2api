package casbinx

import (
	"errors"

	"github.com/casbin/casbin/v3/rbac"
)

// errReadOnly answers every write: nothing changes links through Casbin.
var errReadOnly = errors.New("casbinx: the role manager is read-only")

// roleManager is the read-only rbac.RoleManager of every snapshot. No g row
// is loaded: a resolution asks the matcher with each role subject the
// user's roles name (read by RoleCache at the snapshot's version), so g
// only has to link a subject to itself. There is no role hierarchy, and a
// user subject links to nothing: an Enforce that skipped the roles is
// denied (fail closed).
type roleManager struct{}

var _ rbac.RoleManager = roleManager{}

// HasLink is g in the matcher.
func (roleManager) HasLink(name1, name2 string, _ ...string) (bool, error) {
	return name1 == name2, nil
}

// GetRoles and GetImplicitRoles: roles inherit nothing.
func (roleManager) GetRoles(string, ...string) ([]string, error)         { return nil, nil }
func (roleManager) GetImplicitRoles(string, ...string) ([]string, error) { return nil, nil }

// Clear is called by BuildRoleLinks; there is nothing to clear.
func (roleManager) Clear() error { return nil }

func (roleManager) AddLink(string, string, ...string) error           { return errReadOnly }
func (roleManager) BuildRelationship(string, string, ...string) error { return errReadOnly }
func (roleManager) DeleteLink(string, string, ...string) error        { return errReadOnly }
func (roleManager) DeleteDomain(string) error                         { return errReadOnly }

// GetUsers and GetImplicitUsers would need every user's roles, which no
// snapshot holds.
func (roleManager) GetUsers(string, ...string) ([]string, error)         { return nil, errReadOnly }
func (roleManager) GetImplicitUsers(string, ...string) ([]string, error) { return nil, errReadOnly }

func (roleManager) GetDomains(string) ([]string, error)             { return nil, nil }
func (roleManager) GetAllDomains() ([]string, error)                { return nil, nil }
func (roleManager) PrintRoles() error                               { return nil }
func (roleManager) Match(str, pattern string) bool                  { return str == pattern }
func (roleManager) AddMatchingFunc(string, rbac.MatchingFunc)       {}
func (roleManager) AddDomainMatchingFunc(string, rbac.MatchingFunc) {}
