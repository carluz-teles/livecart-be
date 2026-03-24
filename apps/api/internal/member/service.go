package member

import (
	"context"

	"go.uber.org/zap"
)

type Service struct {
	repo *Repository
	log  *zap.Logger
}

func NewService(repo *Repository, log *zap.Logger) *Service {
	return &Service{repo: repo, log: log}
}

func (s *Service) List(ctx context.Context, storeID string) ([]MemberOutput, error) {
	rows, err := s.repo.List(ctx, storeID)
	if err != nil {
		return nil, err
	}

	members := make([]MemberOutput, len(rows))
	for i, row := range rows {
		members[i] = MemberOutput{
			ID:        row.ID,
			Email:     row.Email,
			Name:      row.Name,
			AvatarURL: row.AvatarURL,
			Role:      row.Role,
			Status:    row.Status,
			JoinedAt:  row.JoinedAt,
			InvitedAt: row.InvitedAt,
		}
	}

	return members, nil
}

func (s *Service) UpdateRole(ctx context.Context, input UpdateMemberRoleInput) (*MemberOutput, error) {
	// Validate role
	if input.Role != RoleAdmin && input.Role != RoleMember {
		return nil, &InvalidRoleError{Role: input.Role}
	}

	row, err := s.repo.UpdateRole(ctx, input.StoreID, input.MemberID, input.Role)
	if err != nil {
		return nil, err
	}

	return &MemberOutput{
		ID:        row.ID,
		Email:     row.Email,
		Name:      row.Name,
		AvatarURL: row.AvatarURL,
		Role:      row.Role,
		Status:    row.Status,
		JoinedAt:  row.JoinedAt,
		InvitedAt: row.InvitedAt,
	}, nil
}

func (s *Service) Remove(ctx context.Context, storeID, memberID, requestingUserID string) error {
	// Prevent self-removal
	if memberID == requestingUserID {
		return &SelfRemovalError{}
	}

	// Get member to check if they're the owner
	member, err := s.repo.GetByID(ctx, storeID, memberID)
	if err != nil {
		return err
	}

	// Cannot remove the owner
	if member.Role == RoleOwner {
		return &CannotRemoveOwnerError{}
	}

	return s.repo.Remove(ctx, storeID, memberID)
}

// Custom errors

type InvalidRoleError struct {
	Role string
}

func (e *InvalidRoleError) Error() string {
	return "invalid role: " + e.Role
}

type SelfRemovalError struct{}

func (e *SelfRemovalError) Error() string {
	return "cannot remove yourself from the store"
}

type CannotRemoveOwnerError struct{}

func (e *CannotRemoveOwnerError) Error() string {
	return "cannot remove the store owner"
}
