package member

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/internal/member/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

// Ensure MembershipLookupAdapter implements httpx.MembershipLookup
var _ httpx.MembershipLookup = (*MembershipLookupAdapter)(nil)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("member"),
	}
}

// List returns all members of a store from local database
func (s *Service) List(ctx context.Context, storeID string) ([]MemberOutput, error) {
	members, err := s.repo.List(ctx, storeID)
	if err != nil {
		return nil, err
	}

	outputs := make([]MemberOutput, len(members))
	for i, m := range members {
		outputs[i] = toMemberOutput(m)
	}

	return outputs, nil
}

func (s *Service) UpdateRole(ctx context.Context, input UpdateMemberRoleInput) (*MemberOutput, error) {
	// Get the member to update
	member, err := s.repo.GetByID(ctx, input.StoreID, input.MemberID)
	if err != nil {
		return nil, err
	}

	// Get the actor (who is making the change)
	actor, err := s.repo.GetByID(ctx, input.StoreID, input.ActorID)
	if err != nil {
		return nil, err
	}

	// Parse the new role
	newRole, err := vo.NewRole(input.Role)
	if err != nil {
		return nil, httpx.ErrUnprocessable("invalid role")
	}

	// Use domain logic to validate
	if err := member.CanChangeRoleTo(newRole, actor); err != nil {
		return nil, httpx.ErrForbidden(err.Error())
	}

	// Persist the change
	updated, err := s.repo.UpdateRole(ctx, input.StoreID, input.MemberID, input.Role)
	if err != nil {
		return nil, err
	}

	s.logger.Info("member role updated",
		zap.String("store_id", input.StoreID),
		zap.String("member_id", input.MemberID),
		zap.String("new_role", input.Role),
		zap.String("updated_by", input.ActorID),
	)

	output := toMemberOutput(updated)
	return &output, nil
}

func (s *Service) Remove(ctx context.Context, input RemoveMemberInput) error {
	// Get the member to remove
	member, err := s.repo.GetByID(ctx, input.StoreID, input.MemberID)
	if err != nil {
		return err
	}

	// Get the actor (who is removing)
	actor, err := s.repo.GetByID(ctx, input.StoreID, input.ActorID)
	if err != nil {
		return err
	}

	// Use domain logic to validate
	if err := member.CanBeRemovedBy(actor); err != nil {
		return httpx.ErrForbidden(err.Error())
	}

	// Persist the removal
	if err := s.repo.Remove(ctx, input.StoreID, input.MemberID); err != nil {
		return err
	}

	s.logger.Info("member removed from store",
		zap.String("store_id", input.StoreID),
		zap.String("member_id", input.MemberID),
		zap.String("removed_by", input.ActorID),
	)

	return nil
}

// toMemberOutput converts a domain Member to a MemberOutput DTO.
func toMemberOutput(m *domain.Member) MemberOutput {
	return MemberOutput{
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

// MemberLookupAdapter implements invitation.MemberLookup interface
type MemberLookupAdapter struct {
	repo *Repository
}

// NewMemberLookupAdapter creates a new adapter for member lookup
func NewMemberLookupAdapter(repo *Repository) *MemberLookupAdapter {
	return &MemberLookupAdapter{repo: repo}
}

// GetMemberNameByID implements invitation.MemberLookup
func (a *MemberLookupAdapter) GetMemberNameByID(ctx context.Context, storeID, memberID string) (string, error) {
	member, err := a.repo.GetByID(ctx, storeID, memberID)
	if err != nil {
		return "", err
	}
	if member.Name() != nil {
		return *member.Name(), nil
	}
	return member.Email().String(), nil // Fallback to email if no name
}

// MembershipLookupAdapter implements invitation.MembershipLookup interface
// This adapter is used to check if a user already has a membership (1 user = 1 store)
type MembershipLookupAdapter struct {
	repo *Repository
}

// NewMembershipLookupAdapter creates a new adapter for membership lookup
func NewMembershipLookupAdapter(repo *Repository) *MembershipLookupAdapter {
	return &MembershipLookupAdapter{repo: repo}
}

// GetMembershipByUserID implements httpx.MembershipLookup
// Returns nil if user has no membership
func (a *MembershipLookupAdapter) GetMembershipByUserID(ctx context.Context, userID string) (httpx.MembershipData, error) {
	info, err := a.repo.GetMembershipByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return info, nil
}

// DeleteMembershipByUserID implements httpx.MembershipLookup
func (a *MembershipLookupAdapter) DeleteMembershipByUserID(ctx context.Context, userID string) error {
	return a.repo.DeleteMembershipByUserID(ctx, userID)
}
