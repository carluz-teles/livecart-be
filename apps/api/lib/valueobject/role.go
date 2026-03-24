package valueobject

import "database/sql/driver"

// Role represents a member's role in a store.
type Role struct {
	value string
}

// Role constants
var (
	RoleOwner  = Role{value: "owner"}
	RoleAdmin  = Role{value: "admin"}
	RoleMember = Role{value: "member"}
)

// validRoles defines all valid role values.
var validRoles = map[string]Role{
	"owner":  RoleOwner,
	"admin":  RoleAdmin,
	"member": RoleMember,
}

// NewRole creates a new Role from a string.
func NewRole(raw string) (Role, error) {
	if raw == "" {
		return Role{}, ErrEmptyRole
	}

	role, ok := validRoles[raw]
	if !ok {
		return Role{}, ErrInvalidRole
	}

	return role, nil
}

// MustNewRole creates a new Role or panics if invalid.
func MustNewRole(raw string) Role {
	r, err := NewRole(raw)
	if err != nil {
		panic(err)
	}
	return r
}

// String returns the role as a string.
func (r Role) String() string {
	return r.value
}

// IsZero returns true if the role is empty.
func (r Role) IsZero() bool {
	return r.value == ""
}

// Equals compares two roles for equality.
func (r Role) Equals(other Role) bool {
	return r.value == other.value
}

// IsOwner returns true if the role is owner.
func (r Role) IsOwner() bool {
	return r.value == RoleOwner.value
}

// IsAdmin returns true if the role is admin.
func (r Role) IsAdmin() bool {
	return r.value == RoleAdmin.value
}

// IsMember returns true if the role is member.
func (r Role) IsMember() bool {
	return r.value == RoleMember.value
}

// CanManageMembers returns true if this role can manage other members.
func (r Role) CanManageMembers() bool {
	return r.IsOwner() || r.IsAdmin()
}

// CanManageStore returns true if this role can manage store settings.
func (r Role) CanManageStore() bool {
	return r.IsOwner()
}

// CanInviteMembers returns true if this role can invite new members.
func (r Role) CanInviteMembers() bool {
	return r.IsOwner() || r.IsAdmin()
}

// IsHigherThan returns true if this role has more permissions than another.
func (r Role) IsHigherThan(other Role) bool {
	return roleHierarchy[r.value] > roleHierarchy[other.value]
}

// IsHigherOrEqual returns true if this role has equal or more permissions.
func (r Role) IsHigherOrEqual(other Role) bool {
	return roleHierarchy[r.value] >= roleHierarchy[other.value]
}

// roleHierarchy defines the permission level of each role.
var roleHierarchy = map[string]int{
	"member": 1,
	"admin":  2,
	"owner":  3,
}

// AssignableRoles returns roles that can be assigned to new members.
// Owner cannot be assigned via API.
func AssignableRoles() []Role {
	return []Role{RoleAdmin, RoleMember}
}

// Value implements driver.Valuer for database serialization.
func (r Role) Value() (driver.Value, error) {
	if r.IsZero() {
		return nil, nil
	}
	return r.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (r *Role) Scan(src any) error {
	if src == nil {
		r.value = ""
		return nil
	}

	switch v := src.(type) {
	case string:
		role, err := NewRole(v)
		if err != nil {
			// Store raw value even if invalid
			r.value = v
			return nil
		}
		*r = role
		return nil
	case []byte:
		role, err := NewRole(string(v))
		if err != nil {
			r.value = string(v)
			return nil
		}
		*r = role
		return nil
	default:
		return ErrInvalidRole
	}
}
