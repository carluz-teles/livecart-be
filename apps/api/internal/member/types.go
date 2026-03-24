package member

import "time"

// ============================================
// Handler layer - Request/Response types
// ============================================

type MemberResponse struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	Name      *string    `json:"name"`
	AvatarURL *string    `json:"avatarUrl"`
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	JoinedAt  time.Time  `json:"joinedAt"`
	InvitedAt *time.Time `json:"invitedAt,omitempty"`
}

type ListMembersResponse struct {
	Data []MemberResponse `json:"data"`
}

type UpdateMemberRoleRequest struct {
	Role string `json:"role" validate:"required,oneof=admin member"`
}

// ============================================
// Service layer - Input/Output types
// ============================================

type MemberOutput struct {
	ID        string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	Status    string
	JoinedAt  time.Time
	InvitedAt *time.Time
}

type UpdateMemberRoleInput struct {
	StoreID  string
	MemberID string
	ActorID  string // The store_user.id of who is making the change
	Role     string
}

type RemoveMemberInput struct {
	StoreID  string
	MemberID string
	ActorID  string // The store_user.id of who is removing
}
