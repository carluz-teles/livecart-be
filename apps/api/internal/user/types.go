package user

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	"livecart/apps/api/internal/billing"
)

// ============================================
// Handler layer - Request/Response types
// ============================================

// SyncUserRequest is the (empty) body for POST /users/sync. Sync derives all its
// data from the authenticated JWT claims, so there are no client-supplied fields;
// Validate() satisfies the httpx.Validatable contract and always passes.
type SyncUserRequest struct{}

// Validate is the syntactic gate. Sync has no body fields, so it is a no-op.
func (r SyncUserRequest) Validate() error {
	return validation.ValidateStruct(&r)
}

// ToInput builds the usecase input from the authenticated claims. The context
// strings (clerk user id, email, name, avatar) are extracted from the JWT by the
// handler and passed in — there are no value objects to construct here, so it
// never fails.
func (r SyncUserRequest) ToInput(clerkUserID, email, name, avatarURL string) (SyncUserInput, error) {
	return SyncUserInput{
		ClerkUserID: clerkUserID,
		Email:       email,
		Name:        name,
		AvatarURL:   avatarURL,
	}, nil
}

// SyncUserResponse - returns user info and single membership (1 user = 1 store)
type SyncUserResponse struct {
	UserID       string                     `json:"userId"`
	ClerkUserID  string                     `json:"clerkUserId"`
	Email        string                     `json:"email"`
	Name         *string                    `json:"name"`
	AvatarURL    *string                    `json:"avatarUrl"`
	Membership   *MembershipResponse        `json:"membership"`             // Single membership (or null)
	State        string                     `json:"state"`                  // "no_store" | "ready"
	Subscription *billing.SubscriptionState `json:"subscription,omitempty"` // paywall state (PRD 007)
}

// MembershipResponse represents a user's membership to a store
type MembershipResponse struct {
	ID           string    `json:"id"`
	StoreID      string    `json:"storeId"`
	StoreName    string    `json:"storeName"`
	StoreSlug    string    `json:"storeSlug"`
	StoreLogoURL *string   `json:"storeLogoUrl"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	Email        string    `json:"email"`
	Name         *string   `json:"name"`
	AvatarURL    *string   `json:"avatarUrl"`
	CreatedAt    time.Time `json:"createdAt"`
}

// NewSyncUserResponse maps the sync usecase output to its API response. This is
// the controller's outbound mapper. Sync's result is intentionally a composite
// read model (user identity + membership + billing/paywall state + store logo)
// that the domain User entity does not model, so it maps from the usecase output
// rather than from a single domain entity.
func NewSyncUserResponse(out *SyncUserOutput) SyncUserResponse {
	var membership *MembershipResponse
	if out.Membership != nil {
		membership = &MembershipResponse{
			ID:           out.Membership.ID,
			StoreID:      out.Membership.StoreID,
			StoreName:    out.Membership.StoreName,
			StoreSlug:    out.Membership.StoreSlug,
			StoreLogoURL: out.Membership.StoreLogoURL,
			Role:         out.Membership.Role,
			Status:       out.Membership.Status,
			Email:        out.Membership.Email,
			Name:         out.Membership.Name,
			AvatarURL:    out.Membership.AvatarURL,
			CreatedAt:    out.Membership.CreatedAt,
		}
	}

	return SyncUserResponse{
		UserID:       out.UserID,
		ClerkUserID:  out.ClerkUserID,
		Email:        out.Email,
		Name:         out.Name,
		AvatarURL:    out.AvatarURL,
		Membership:   membership,
		State:        out.State,
		Subscription: out.Subscription,
	}
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
	UserID       string
	ClerkUserID  string
	Email        string
	Name         *string
	AvatarURL    *string
	Membership   *MembershipOutput // Single membership (or nil)
	State        string            // "no_store" | "ready"
	Subscription *billing.SubscriptionState
}

// MembershipOutput - membership data from service
type MembershipOutput struct {
	ID           string
	StoreID      string
	UserID       string
	StoreName    string
	StoreSlug    string
	StoreLogoURL *string
	Role         string
	Status       string
	Email        string
	Name         *string
	AvatarURL    *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
	ID           string
	StoreID      string
	UserID       string
	Email        string
	Name         *string
	AvatarURL    *string
	Role         string
	Status       string
	StoreName    string
	StoreSlug    string
	StoreLogoURL *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateMembershipParams - params for creating membership
type CreateMembershipParams struct {
	StoreID   string
	UserID    string
	Role      string
	InvitedBy *string
}
