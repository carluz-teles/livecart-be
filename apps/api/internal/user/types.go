package user

import "time"

// ============================================
// Handler layer - Request/Response types
// ============================================

// SyncUserRequest - optional body for sync
type SyncUserRequest struct {
	// No required fields - sync uses JWT claims
}

// SyncUserResponse - returns all memberships and state
type SyncUserResponse struct {
	ClerkUserID          string               `json:"clerkUserId"`
	Memberships          []MembershipResponse `json:"memberships"`
	LastAccessedStoreID  *string              `json:"lastAccessedStoreId"`
	State                string               `json:"state"` // "no_store" | "ready"
}

// MembershipResponse represents a user's membership to a store
type MembershipResponse struct {
	ID             string    `json:"id"`
	StoreID        string    `json:"storeId"`
	StoreName      string    `json:"storeName"`
	StoreSlug      string    `json:"storeSlug"`
	ClerkOrgID     string    `json:"clerkOrgId"`
	Role           string    `json:"role"`
	Status         string    `json:"status"`
	Email          string    `json:"email"`
	Name           *string   `json:"name"`
	AvatarURL      *string   `json:"avatarUrl"`
	LastAccessedAt *time.Time `json:"lastAccessedAt"`
	CreatedAt      time.Time `json:"createdAt"`
}

// SelectStoreRequest - for changing active store
type SelectStoreRequest struct {
	StoreID string `json:"storeId" validate:"required,uuid"`
}

// GetMeResponse - current user info for a specific store context
type GetMeResponse struct {
	ID        string    `json:"id"`
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

// GetUserStoresResponse - list of stores user belongs to
type GetUserStoresResponse struct {
	Stores []MembershipResponse `json:"stores"`
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

// SyncUserOutput - output from sync service
type SyncUserOutput struct {
	ClerkUserID         string
	Memberships         []MembershipOutput
	LastAccessedStoreID *string
	State               string // "no_store" | "ready"
}

// MembershipOutput - membership data from service
type MembershipOutput struct {
	ID             string
	StoreID        string
	StoreName      string
	StoreSlug      string
	ClerkOrgID     string
	Role           string
	Status         string
	Email          string
	Name           *string
	AvatarURL      *string
	LastAccessedAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UserOutput - user info for a specific store context
type UserOutput struct {
	ID        string
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

// UpdateUserInput - input for updating user
type UpdateUserInput struct {
	ClerkUserID string
	StoreID     string
	Email       string
	Name        string
	AvatarURL   string
}

// ============================================
// Repository layer types
// ============================================

// MembershipRow - row from memberships table with store info
type MembershipRow struct {
	ID             string
	StoreID        string
	ClerkUserID    string
	Email          string
	Name           *string
	AvatarURL      *string
	Role           string
	Status         string
	StoreName      string
	StoreSlug      string
	ClerkOrgID     string
	LastAccessedAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CreateMembershipParams - params for creating membership
type CreateMembershipParams struct {
	StoreID     string
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
	Role        string
	InvitedBy   *string
}
