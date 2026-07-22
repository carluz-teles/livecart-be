package invitation

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"

	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// Handler layer - Request types (ozzo Validate + ToInput)
// ============================================

type CreateInvitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// Validate is the syntactic gate (ozzo). owner is not settable via the API.
func (r CreateInvitationRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Email, validation.Required, is.Email),
		validation.Field(&r.Role, validation.Required, validation.In("admin", "member")),
	)
}

// ToInput builds the usecase input, constructing the value objects (semantic
// validation). Returns error so an invalid VO surfaces as 422 via the ErrorHandler.
func (r CreateInvitationRequest) ToInput(storeIDStr, inviterIDStr string) (CreateInvitationInput, error) {
	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return CreateInvitationInput{}, httpx.ErrUnprocessable("invalid store ID")
	}
	email, err := vo.NewEmail(r.Email)
	if err != nil {
		return CreateInvitationInput{}, httpx.ErrUnprocessable("invalid email format")
	}
	role, err := vo.NewRole(r.Role)
	if err != nil {
		return CreateInvitationInput{}, httpx.ErrUnprocessable("invalid role")
	}
	inviterID, err := vo.NewMemberID(inviterIDStr)
	if err != nil {
		return CreateInvitationInput{}, httpx.ErrUnprocessable("invalid inviter ID")
	}
	return CreateInvitationInput{
		StoreID:   storeID,
		InviterID: inviterID,
		Email:     email,
		Role:      role,
	}, nil
}

type AcceptInvitationRequest struct {
	Token string `json:"token"`
}

// Validate is the syntactic gate (ozzo).
func (r AcceptInvitationRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Token, validation.Required),
	)
}

// ToInput builds the usecase input, constructing the Email value object from the
// authenticated claims. Returns error so an invalid VO surfaces as 422.
func (r AcceptInvitationRequest) ToInput(clerkUserID, emailStr string) (AcceptInvitationInput, error) {
	email, err := vo.NewEmail(emailStr)
	if err != nil {
		return AcceptInvitationInput{}, httpx.ErrUnprocessable("invalid email: " + emailStr)
	}
	return AcceptInvitationInput{
		Token:       r.Token,
		ClerkUserID: clerkUserID,
		Email:       email,
	}, nil
}

// ============================================
// Handler layer - Response types (mappers from domain.Invitation)
// ============================================

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

// NewInvitationResponse maps a domain Invitation to its API response. This is the
// controller's outbound mapper: presentation knows the domain; the domain never
// knows the response.
func NewInvitationResponse(inv *domain.Invitation) InvitationResponse {
	return InvitationResponse{
		ID:          inv.ID().String(),
		Email:       inv.Email().String(),
		Role:        inv.Role().String(),
		Status:      inv.Status().String(),
		InviterName: inv.InviterName(),
		ExpiresAt:   inv.ExpiresAt(),
		AcceptedAt:  inv.AcceptedAt(),
		CreatedAt:   inv.CreatedAt(),
	}
}

type ListInvitationsResponse struct {
	Data []InvitationResponse `json:"data"`
}

// NewListInvitationsResponse maps a slice of Invitation entities to the list response.
func NewListInvitationsResponse(invitations []*domain.Invitation) ListInvitationsResponse {
	data := make([]InvitationResponse, len(invitations))
	for i, inv := range invitations {
		data[i] = NewInvitationResponse(inv)
	}
	return ListInvitationsResponse{Data: data}
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

// NewInvitationDetailsResponse maps a domain Invitation (loaded by token) to the
// public accept-page response.
func NewInvitationDetailsResponse(inv *domain.Invitation) InvitationDetailsResponse {
	return InvitationDetailsResponse{
		ID:          inv.ID().String(),
		Email:       inv.Email().String(),
		Role:        inv.Role().String(),
		Status:      inv.Status().String(),
		StoreName:   inv.StoreName(),
		StoreSlug:   inv.StoreSlug(),
		InviterName: inv.InviterName(),
		ExpiresAt:   inv.ExpiresAt(),
		CreatedAt:   inv.CreatedAt(),
	}
}

type AcceptInvitationResponse struct {
	StoreID   string `json:"storeId"`
	StoreName string `json:"storeName"`
	StoreSlug string `json:"storeSlug"`
	Role      string `json:"role"`
}

// NewAcceptInvitationResponse maps the accepted Invitation to the accept response.
func NewAcceptInvitationResponse(inv *domain.Invitation) AcceptInvitationResponse {
	return AcceptInvitationResponse{
		StoreID:   inv.StoreID().String(),
		StoreName: inv.StoreName(),
		StoreSlug: inv.StoreSlug(),
		Role:      inv.Role().String(),
	}
}

// ============================================
// Service layer - Input types (usecase returns *domain.Invitation)
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

type UserInfo struct {
	Email     string
	Name      string
	AvatarURL string
}
