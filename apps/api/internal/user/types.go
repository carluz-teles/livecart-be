package user

import "time"

// ============================================
// Handler layer - Request/Response types
// ============================================

// SyncUserRequest - optional body for sync
type SyncUserRequest struct {
	// No required fields - sync uses JWT claims
}

// SyncUserResponse - returns user info and single membership (1 user = 1 store)
type SyncUserResponse struct {
	UserID      string              `json:"userId"`
	ClerkUserID string              `json:"clerkUserId"`
	Email       string              `json:"email"`
	Name        *string             `json:"name"`
	AvatarURL   *string             `json:"avatarUrl"`
	Membership  *MembershipResponse `json:"membership"` // Single membership (or null)
	State       string              `json:"state"`      // "no_store" | "ready"
}

// MembershipResponse represents a user's membership to a store
type MembershipResponse struct {
	ID        string    `json:"id"`
	StoreID   string    `json:"storeId"`
	StoreName string    `json:"storeName"`
	StoreSlug string    `json:"storeSlug"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	Email     string    `json:"email"`
	Name      *string   `json:"name"`
	AvatarURL *string   `json:"avatarUrl"`
	CreatedAt time.Time `json:"createdAt"`
}

// GetMeResponse - current user info for a specific store context
type GetMeResponse struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	StoreID   string    `json:"storeId"`
	Email     string    `json:"email"`
	Name      *string   `json:"name"`
	AvatarURL *string   `json:"avatarUrl"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	StoreName string    `json:"storeName"`
	StoreSlug string    `json:"storeSlug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ============================================
// Service layer types
// ============================================

// SyncUserInput - input for sync service
type SyncUserInput struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
}

// SyncUserOutput - output from sync service (single membership)
type SyncUserOutput struct {
	UserID      string
	ClerkUserID string
	Email       string
	Name        *string
	AvatarURL   *string
	Membership  *MembershipOutput // Single membership (or nil)
	State       string            // "no_store" | "ready"
}

// MembershipOutput - membership data from service
type MembershipOutput struct {
	ID        string
	StoreID   string
	UserID    string
	StoreName string
	StoreSlug string
	Role      string
	Status    string
	Email     string
	Name      *string
	AvatarURL *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserOutput - user info for a specific store context
type UserOutput struct {
	ID        string
	UserID    string
	StoreID   string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	Status    string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserInfo - basic user info from users table
type UserInfo struct {
	ID        string
	ClerkID   string
	Email     string
	Name      *string
	AvatarURL *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpdateUserInput - input for updating user
type UpdateUserInput struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
}

// ============================================
// Repository layer types
// ============================================

// UserRow - row from users table
type UserRow struct {
	ID        string
	ClerkID   string
	Email     string
	Name      *string
	AvatarURL *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MembershipRow - row from memberships table with store and user info
type MembershipRow struct {
	ID        string
	StoreID   string
	UserID    string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	Status    string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateMembershipParams - params for creating membership
type CreateMembershipParams struct {
	StoreID   string
	UserID    string
	Role      string
	InvitedBy *string
}
