package invitation

import "time"

// Roles
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Invitation statuses
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusExpired  = "expired"
	StatusRevoked  = "revoked"
)

// Handler layer - Request/Response types

type CreateInvitationRequest struct {
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role" validate:"required,oneof=admin member"`
}

type InvitationResponse struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	InviterName *string    `json:"inviterName"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	AcceptedAt  *time.Time `json:"acceptedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

type ListInvitationsResponse struct {
	Data []InvitationResponse `json:"data"`
}

type InvitationDetailsResponse struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	StoreName   string    `json:"storeName"`
	StoreSlug   string    `json:"storeSlug"`
	InviterName *string   `json:"inviterName"`
	ExpiresAt   time.Time `json:"expiresAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

type AcceptInvitationRequest struct {
	Token string `json:"token" validate:"required"`
}

// Service layer

type CreateInvitationInput struct {
	StoreID     string
	InviterID   string // store_user.id of the person inviting
	Email       string
	Role        string
}

type CreateInvitationOutput struct {
	ID        string
	Email     string
	Role      string
	Token     string
	Status    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

type InvitationOutput struct {
	ID          string
	StoreID     string
	Email       string
	Role        string
	Token       string
	Status      string
	InviterName *string
	ExpiresAt   time.Time
	AcceptedAt  *time.Time
	CreatedAt   time.Time
}

type InvitationDetailsOutput struct {
	ID          string
	StoreID     string
	Email       string
	Role        string
	Status      string
	StoreName   string
	StoreSlug   string
	InviterName *string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

type AcceptInvitationInput struct {
	Token       string
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
}

type AcceptInvitationOutput struct {
	StoreID   string
	StoreName string
	StoreSlug string
	Role      string
}

// Repository layer

type InvitationRow struct {
	ID          string
	StoreID     string
	Email       string
	Role        string
	Token       string
	InvitedBy   string
	Status      string
	InviterName *string
	StoreName   string
	StoreSlug   string
	ExpiresAt   time.Time
	AcceptedAt  *time.Time
	CreatedAt   time.Time
}
