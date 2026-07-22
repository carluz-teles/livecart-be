package member

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	"livecart/apps/api/internal/member/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

// ============================================
// Handler layer - Request/Response types
// ============================================

type MemberResponse struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Email     string     `json:"email"`
	Name      *string    `json:"name"`
	AvatarURL *string    `json:"avatarUrl"`
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	JoinedAt  *time.Time `json:"joinedAt,omitempty"`
	InvitedAt *time.Time `json:"invitedAt,omitempty"`
}

// NewMemberResponse maps a domain Member entity to its API response. This is the
// controller's outbound mapper: presentation knows the domain; the domain never
// knows the response.
func NewMemberResponse(m *domain.Member) MemberResponse {
	return MemberResponse{
		ID:        m.ID().String(),
		UserID:    m.UserID(),
		Email:     m.Email().String(),
		Name:      m.Name(),
		AvatarURL: m.AvatarURL(),
		Role:      m.Role().String(),
		Status:    m.Status().String(),
		JoinedAt:  m.JoinedAt(),
		InvitedAt: m.InvitedAt(),
	}
}

type ListMembersResponse struct {
	Data []MemberResponse `json:"data"`
}

// NewListMembersResponse maps a slice of Member entities to the list response.
func NewListMembersResponse(members []*domain.Member) ListMembersResponse {
	data := make([]MemberResponse, len(members))
	for i, m := range members {
		data[i] = NewMemberResponse(m)
	}
	return ListMembersResponse{Data: data}
}

type UpdateMemberRoleRequest struct {
	Role string `json:"role"`
}

// Validate is the syntactic gate (ozzo). owner is not settable via the API.
func (r UpdateMemberRoleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Role, validation.Required, validation.In("admin", "member")),
	)
}

// ToInput builds the usecase input, constructing the Role value object (semantic
// validation). Returns error so an invalid VO surfaces as 422 via the ErrorHandler.
func (r UpdateMemberRoleRequest) ToInput(storeID, memberID, actorID string) (UpdateMemberRoleInput, error) {
	role, err := vo.NewRole(r.Role)
	if err != nil {
		return UpdateMemberRoleInput{}, httpx.ErrUnprocessable("invalid role")
	}
	return UpdateMemberRoleInput{
		StoreID:  storeID,
		MemberID: memberID,
		ActorID:  actorID,
		Role:     role,
	}, nil
}

// ============================================
// Service layer - Input types (usecase returns *domain.Member)
// ============================================

type UpdateMemberRoleInput struct {
	StoreID  string
	MemberID string
	ActorID  string // The store_user.id of who is making the change
	Role     vo.Role
}

type RemoveMemberInput struct {
	StoreID  string
	MemberID string
	ActorID  string // The store_user.id of who is removing
}
