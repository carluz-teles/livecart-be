package domain

import (
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// User represents a user in the system (linked to a store via store_user).
type User struct {
	id          vo.MemberID
	storeID     vo.StoreID
	clerkUserID string
	email       vo.Email
	name        *string
	avatarURL   *string
	role        vo.Role
	status      string // active, pending, removed
	storeName   string
	storeSlug   string
	createdAt   time.Time
	updatedAt   time.Time
}

// Reconstruct rebuilds a User from persistence data.
func Reconstruct(
	id vo.MemberID,
	storeID vo.StoreID,
	clerkUserID string,
	email vo.Email,
	name *string,
	avatarURL *string,
	role vo.Role,
	status string,
	storeName string,
	storeSlug string,
	createdAt time.Time,
	updatedAt time.Time,
) *User {
	return &User{
		id:          id,
		storeID:     storeID,
		clerkUserID: clerkUserID,
		email:       email,
		name:        name,
		avatarURL:   avatarURL,
		role:        role,
		status:      status,
		storeName:   storeName,
		storeSlug:   storeSlug,
		createdAt:   createdAt,
		updatedAt:   updatedAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (u *User) ID() vo.MemberID      { return u.id }
func (u *User) StoreID() vo.StoreID  { return u.storeID }
func (u *User) ClerkUserID() string  { return u.clerkUserID }
func (u *User) Email() vo.Email      { return u.email }
func (u *User) Name() *string        { return u.name }
func (u *User) AvatarURL() *string   { return u.avatarURL }
func (u *User) Role() vo.Role        { return u.role }
func (u *User) Status() string       { return u.status }
func (u *User) StoreName() string    { return u.storeName }
func (u *User) StoreSlug() string    { return u.storeSlug }
func (u *User) CreatedAt() time.Time { return u.createdAt }
func (u *User) UpdatedAt() time.Time { return u.updatedAt }

// ============================================
// Business Rules
// ============================================

// IsActive returns true if the user is active.
func (u *User) IsActive() bool {
	return u.status == "active"
}

// IsPending returns true if the user is pending.
func (u *User) IsPending() bool {
	return u.status == "pending"
}

// IsOwner returns true if the user is an owner.
func (u *User) IsOwner() bool {
	return u.role.IsOwner()
}

// IsAdmin returns true if the user is an admin.
func (u *User) IsAdmin() bool {
	return u.role.IsAdmin()
}

// CanManageMembers returns true if the user can manage store members.
func (u *User) CanManageMembers() bool {
	return u.role.CanManageMembers()
}

// BelongsToStore checks if the user belongs to the given store.
func (u *User) BelongsToStore(storeID vo.StoreID) bool {
	return u.storeID.Equals(storeID)
}

// ============================================
// State Changes
// ============================================

// UpdateProfile updates the user's profile information.
func (u *User) UpdateProfile(email vo.Email, name, avatarURL *string) {
	u.email = email
	u.name = name
	u.avatarURL = avatarURL
	u.updatedAt = time.Now()
}

// ChangeRole changes the user's role.
func (u *User) ChangeRole(role vo.Role) {
	u.role = role
	u.updatedAt = time.Now()
}

// Activate activates the user.
func (u *User) Activate() {
	u.status = "active"
	u.updatedAt = time.Now()
}

// Remove marks the user as removed.
func (u *User) Remove() {
	u.status = "removed"
	u.updatedAt = time.Now()
}
