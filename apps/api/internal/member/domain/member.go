package domain

import (
	"errors"
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// Domain errors
var (
	ErrCannotRemoveOwner      = errors.New("cannot remove the store owner")
	ErrCannotRemoveSelf       = errors.New("cannot remove yourself from the store")
	ErrInsufficientPermission = errors.New("insufficient permission for this operation")
	ErrCannotChangeOwnerRole  = errors.New("cannot change the owner's role")
	ErrMemberNotActive        = errors.New("member is not active")
)

// Member represents a user's membership in a store.
type Member struct {
	id        vo.MemberID
	storeID   vo.StoreID
	userID    string // Internal user UUID
	email     vo.Email
	name      *string
	avatarURL *string
	role      vo.Role
	status    MemberStatus
	invitedBy *vo.MemberID
	joinedAt  *time.Time
	invitedAt *time.Time
}

// NewMember creates a new Member entity.
func NewMember(
	id vo.MemberID,
	storeID vo.StoreID,
	userID string,
	email vo.Email,
	role vo.Role,
	status MemberStatus,
	joinedAt *time.Time,
) *Member {
	return &Member{
		id:       id,
		storeID:  storeID,
		userID:   userID,
		email:    email,
		role:     role,
		status:   status,
		joinedAt: joinedAt,
	}
}

// Reconstruct rebuilds a Member from persistence data.
func Reconstruct(
	id vo.MemberID,
	storeID vo.StoreID,
	userID string,
	email vo.Email,
	name *string,
	avatarURL *string,
	role vo.Role,
	status MemberStatus,
	invitedBy *vo.MemberID,
	joinedAt *time.Time,
	invitedAt *time.Time,
) *Member {
	return &Member{
		id:        id,
		storeID:   storeID,
		userID:    userID,
		email:     email,
		name:      name,
		avatarURL: avatarURL,
		role:      role,
		status:    status,
		invitedBy: invitedBy,
		joinedAt:  joinedAt,
		invitedAt: invitedAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (m *Member) ID() vo.MemberID        { return m.id }
func (m *Member) StoreID() vo.StoreID    { return m.storeID }
func (m *Member) UserID() string         { return m.userID }
func (m *Member) Email() vo.Email        { return m.email }
func (m *Member) Name() *string          { return m.name }
func (m *Member) AvatarURL() *string     { return m.avatarURL }
func (m *Member) Role() vo.Role          { return m.role }
func (m *Member) Status() MemberStatus   { return m.status }
func (m *Member) InvitedBy() *vo.MemberID { return m.invitedBy }
func (m *Member) JoinedAt() *time.Time   { return m.joinedAt }
func (m *Member) InvitedAt() *time.Time  { return m.invitedAt }

// ============================================
// Business Rules
// ============================================

// IsOwner returns true if the member is the store owner.
func (m *Member) IsOwner() bool {
	return m.role.IsOwner()
}

// IsAdmin returns true if the member is an admin.
func (m *Member) IsAdmin() bool {
	return m.role.IsAdmin()
}

// IsActive returns true if the member is active.
func (m *Member) IsActive() bool {
	return m.status.IsActive()
}

// CanManageMembers returns true if this member can manage other members.
func (m *Member) CanManageMembers() bool {
	return m.status.IsActive() && m.role.CanManageMembers()
}

// CanInviteMembers returns true if this member can invite new members.
func (m *Member) CanInviteMembers() bool {
	return m.status.IsActive() && m.role.CanInviteMembers()
}

// CanBeRemovedBy checks if this member can be removed by the actor.
// Returns nil if allowed, error otherwise.
func (m *Member) CanBeRemovedBy(actor *Member) error {
	// Cannot remove the owner
	if m.IsOwner() {
		return ErrCannotRemoveOwner
	}

	// Cannot remove yourself
	if m.id.Equals(actor.id) {
		return ErrCannotRemoveSelf
	}

	// Actor must have permission to manage members
	if !actor.CanManageMembers() {
		return ErrInsufficientPermission
	}

	return nil
}

// CanChangeRoleTo checks if this member's role can be changed by the actor.
// Returns nil if allowed, error otherwise.
func (m *Member) CanChangeRoleTo(newRole vo.Role, actor *Member) error {
	// Cannot change owner's role
	if m.IsOwner() {
		return ErrCannotChangeOwnerRole
	}

	// Actor must have permission to manage members
	if !actor.CanManageMembers() {
		return ErrInsufficientPermission
	}

	// Cannot assign owner role via this method
	if newRole.IsOwner() {
		return ErrInsufficientPermission
	}

	// Member must be active
	if !m.IsActive() {
		return ErrMemberNotActive
	}

	return nil
}

// ============================================
// State Changes
// ============================================

// ChangeRole changes the member's role.
// This should only be called after CanChangeRoleTo validation.
func (m *Member) ChangeRole(newRole vo.Role) {
	m.role = newRole
}

// UpdateProfile updates the member's profile information.
func (m *Member) UpdateProfile(name *string, avatarURL *string) {
	m.name = name
	m.avatarURL = avatarURL
}

// Activate activates the member (e.g., after accepting invitation).
func (m *Member) Activate() {
	m.status = StatusActive
}

// Remove marks the member as removed.
func (m *Member) Remove() {
	m.status = StatusRemoved
}
