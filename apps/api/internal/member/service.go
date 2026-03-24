package member

import (
	"context"

	"go.uber.org/zap"
)

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
	row, err := s.repo.UpdateRole(ctx, input.StoreID, input.MemberID, input.Role)
	if err != nil {
		return nil, err
	}

	s.logger.Info("member role updated",
		zap.String("store_id", input.StoreID),
		zap.String("member_id", input.MemberID),
		zap.String("new_role", input.Role),
	)

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

	err = s.repo.Remove(ctx, storeID, memberID)
	if err != nil {
		return err
	}

	s.logger.Info("member removed from store",
		zap.String("store_id", storeID),
		zap.String("member_id", memberID),
		zap.String("removed_by", requestingUserID),
	)

	return nil
}

// Custom errors

type SelfRemovalError struct{}

func (e *SelfRemovalError) Error() string {
	return "cannot remove yourself from the store"
}

type CannotRemoveOwnerError struct{}

func (e *CannotRemoveOwnerError) Error() string {
	return "cannot remove the store owner"
}
