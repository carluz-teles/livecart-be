package member

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/member/domain"
	"livecart/apps/api/internal/store"
	"livecart/apps/api/lib/clerk"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo      *Repository
	storeRepo *store.Repository
	clerkSDK  *clerk.SDK
	logger    *zap.Logger
}

func NewService(repo *Repository, storeRepo *store.Repository, clerkSDK *clerk.SDK, logger *zap.Logger) *Service {
	return &Service{
		repo:      repo,
		storeRepo: storeRepo,
		clerkSDK:  clerkSDK,
		logger:    logger.Named("member"),
	}
}

// List returns all members of a store
// If store has clerk_org_id, fetches from Clerk; otherwise from local database
func (s *Service) List(ctx context.Context, storeID string) ([]MemberOutput, error) {
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting store: %w", err)
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		memberList, err := s.clerkSDK.ListMembers(ctx, storeData.ClerkOrgID)
		if err != nil {
			s.logger.Error("failed to list clerk members, falling back to local",
				zap.Error(err),
				zap.String("org_id", storeData.ClerkOrgID),
			)
			// Fallback to local
			return s.listLocal(ctx, storeID)
		}

		outputs := make([]MemberOutput, len(memberList.OrganizationMemberships))
		for i, m := range memberList.OrganizationMemberships {
			// Convert Clerk role to local role
			role := "member"
			if m.Role == "org:admin" {
				role = "admin"
			}

			// Get user name from public data
			name := ""
			if m.PublicUserData != nil {
				if m.PublicUserData.FirstName != nil {
					name = *m.PublicUserData.FirstName
				}
				if m.PublicUserData.LastName != nil {
					if name != "" {
						name += " "
					}
					name += *m.PublicUserData.LastName
				}
			}

			// Get avatar and email
			avatarURL := ""
			email := ""
			if m.PublicUserData != nil {
				if m.PublicUserData.ImageURL != nil {
					avatarURL = *m.PublicUserData.ImageURL
				}
				email = m.PublicUserData.Identifier // Identifier is the email
			}

			outputs[i] = MemberOutput{
				ID:        m.ID,
				Email:     email,
				Name:      &name,
				AvatarURL: &avatarURL,
				Role:      role,
				Status:    "active",
				JoinedAt:  time.Unix(m.CreatedAt, 0),
			}
		}
		return outputs, nil
	}

	// Fallback to local database
	return s.listLocal(ctx, storeID)
}

// listLocal lists members from local database
func (s *Service) listLocal(ctx context.Context, storeID string) ([]MemberOutput, error) {
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
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, input.StoreID)
	if err != nil {
		return nil, fmt.Errorf("getting store: %w", err)
	}

	// Convert role to Clerk format
	clerkRole := "org:member"
	if input.Role == "admin" {
		clerkRole = "org:admin"
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		// Note: input.MemberID is the Clerk user ID when using Clerk
		_, err := s.clerkSDK.UpdateMemberRole(ctx, storeData.ClerkOrgID, input.MemberID, clerkRole)
		if err != nil {
			s.logger.Error("failed to update clerk member role, trying local",
				zap.Error(err),
				zap.String("org_id", storeData.ClerkOrgID),
				zap.String("member_id", input.MemberID),
			)
			// Try local as fallback
		} else {
			s.logger.Info("member role updated via Clerk",
				zap.String("clerk_org_id", storeData.ClerkOrgID),
				zap.String("member_id", input.MemberID),
				zap.String("new_role", clerkRole),
			)
			// Return a basic output
			return &MemberOutput{
				ID:   input.MemberID,
				Role: input.Role,
			}, nil
		}
	}

	// Local database flow
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

	s.logger.Info("member role updated (local)",
		zap.String("store_id", input.StoreID),
		zap.String("member_id", input.MemberID),
		zap.String("new_role", input.Role),
		zap.String("updated_by", input.ActorID),
	)

	output := toMemberOutput(updated)
	return &output, nil
}

func (s *Service) Remove(ctx context.Context, input RemoveMemberInput) error {
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, input.StoreID)
	if err != nil {
		return fmt.Errorf("getting store: %w", err)
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		// Note: input.MemberID is the Clerk user ID when using Clerk
		_, err := s.clerkSDK.RemoveMember(ctx, storeData.ClerkOrgID, input.MemberID)
		if err != nil {
			s.logger.Error("failed to remove clerk member, trying local",
				zap.Error(err),
				zap.String("org_id", storeData.ClerkOrgID),
				zap.String("member_id", input.MemberID),
			)
			// Try local as fallback
		} else {
			s.logger.Info("member removed via Clerk",
				zap.String("clerk_org_id", storeData.ClerkOrgID),
				zap.String("member_id", input.MemberID),
			)
			return nil
		}
	}

	// Local database flow
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

	s.logger.Info("member removed from store (local)",
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
		Email:     m.Email().String(),
		Name:      m.Name(),
		AvatarURL: m.AvatarURL(),
		Role:      m.Role().String(),
		Status:    m.Status().String(),
		JoinedAt:  m.JoinedAt(),
		InvitedAt: m.InvitedAt(),
	}
}
