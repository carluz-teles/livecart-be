package member

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/member/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
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

// List returns all members of a store as domain entities.
func (s *Service) List(ctx context.Context, storeID string) ([]*domain.Member, error) {
	return s.repo.List(ctx, storeID)
}

func (s *Service) UpdateRole(ctx context.Context, input UpdateMemberRoleInput) (*domain.Member, error) {
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

	// Use domain logic to validate the transition (the Role VO was built in ToInput).
	if err := member.CanChangeRoleTo(input.Role, actor); err != nil {
		return nil, httpx.ErrForbidden(err.Error())
	}

	// Persist the change
	updated, err := s.repo.UpdateRole(ctx, input.StoreID, input.MemberID, input.Role.String())
	if err != nil {
		return nil, err
	}

	// Group K: member.role_changed fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberRoleChanged,
		"member.role_changed:"+input.MemberID+":"+input.Role.String(), struct {
			MemberID string `json:"member_id"`
			StoreID  string `json:"store_id"`
			Role     string `json:"role"`
			ActorID  string `json:"actor_id"`
		}{MemberID: input.MemberID, StoreID: input.StoreID, Role: input.Role.String(), ActorID: input.ActorID})

	logger.From(ctx, s.logger).Info("member role updated",
		zap.String("member_id", input.MemberID),
		zap.String("new_role", input.Role.String()),
		zap.String("updated_by", input.ActorID),
	)

	return updated, nil
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

	// Group K: member.removed fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberRemoved,
		"member.removed:"+input.MemberID, struct {
			MemberID string `json:"member_id"`
			StoreID  string `json:"store_id"`
			ActorID  string `json:"actor_id"`
		}{MemberID: input.MemberID, StoreID: input.StoreID, ActorID: input.ActorID})

	logger.From(ctx, s.logger).Info("member removed from store",
		zap.String("member_id", input.MemberID),
		zap.String("removed_by", input.ActorID),
	)

	return nil
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
