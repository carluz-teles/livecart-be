package invitation

import (
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// Handler layer - Request/Response types
// ============================================

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

// ============================================
// Service layer - Input/Output types
// ============================================

type CreateInvitationInput struct {
	StoreID   vo.StoreID
	InviterID vo.MemberID
	Email     vo.Email
	Role      vo.Role
}

type ResendInvitationInput struct {
	StoreID      vo.StoreID
	InvitationID vo.InvitationID
	InviterID    vo.MemberID
}

type AcceptInvitationInput struct {
	Token       string
	ClerkUserID string // Clerk user ID from JWT
	Email       vo.Email
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

type AcceptInvitationOutput struct {
	StoreID   string
	StoreName string
	StoreSlug string
	Role      string
}

type UserInfo struct {
	Email     string
	Name      string
	AvatarURL string
}
