package invitation

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/email"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

// UserLookup interface to look up users
type UserLookup interface {
	GetUserByEmail(ctx context.Context, email string) (userID string, err error)
	GetUserIDByClerkID(ctx context.Context, clerkUserID string) (userID string, err error)
}


// StoreLookup interface to look up store info
type StoreLookup interface {
	GetStoreNameByID(ctx context.Context, storeID string) (storeName string, err error)
}

// MemberLookup interface to look up member info
type MemberLookup interface {
	GetMemberNameByID(ctx context.Context, storeID, memberID string) (name string, err error)
}

type Service struct {
	repo             *Repository
	emailer          *email.Client
	userLookup       UserLookup
	storeLookup      StoreLookup
	memberLookup     MemberLookup
	membershipLookup httpx.MembershipLookup
	logger           *zap.Logger
}

func NewService(repo *Repository, emailer *email.Client, userLookup UserLookup, storeLookup StoreLookup, memberLookup MemberLookup, membershipLookup httpx.MembershipLookup, logger *zap.Logger) *Service {
	return &Service{
		repo:             repo,
		emailer:          emailer,
		userLookup:       userLookup,
		storeLookup:      storeLookup,
		memberLookup:     memberLookup,
		membershipLookup: membershipLookup,
		logger:           logger.Named("invitation"),
	}
}

// Create creates a new invitation and sends email via SendGrid
func (s *Service) Create(ctx context.Context, input CreateInvitationInput) (*InvitationOutput, error) {
	// Check if invitation already exists
	existing, err := s.repo.GetByEmail(ctx, input.StoreID, input.Email)
	if err == nil && existing.IsPending() {
		return nil, httpx.ErrConflict("invitation already exists for this email")
	}

	// Look up store name and inviter name for email template
	storeName, err := s.storeLookup.GetStoreNameByID(ctx, input.StoreID.String())
	if err != nil {
		s.logger.Warn("could not look up store name", zap.Error(err))
		storeName = "Store" // Fallback
	}

	inviterName, err := s.memberLookup.GetMemberNameByID(ctx, input.StoreID.String(), input.InviterID.String())
	if err != nil {
		s.logger.Warn("could not look up inviter name", zap.Error(err))
		inviterName = "A team member" // Fallback
	}

	// Create new invitation via domain factory
	inv, err := domain.NewInvitation(input.StoreID, input.Email, input.Role, input.InviterID)
	if err != nil {
		return nil, fmt.Errorf("creating invitation: %w", err)
	}

	// Save to repository
	if err := s.repo.Save(ctx, inv); err != nil {
		return nil, err
	}

	// Send invitation email via SendGrid
	acceptURL := fmt.Sprintf("%s/accept-invite?token=%s", config.FrontendURL.StringOr("http://localhost:3000"), inv.Token().String())

	err = s.emailer.SendInvitation(ctx, email.InvitationEmailInput{
		ToEmail:     input.Email.String(),
		ToName:      "", // We don't have the invitee's name yet
		StoreName:   storeName,
		InviterName: inviterName,
		Role:        input.Role.String(),
		AcceptURL:   acceptURL,
		ExpiresAt:   inv.ExpiresAt(),
	})
	if err != nil {
		s.logger.Error("failed to send invitation email",
			zap.Error(err),
			zap.String("email", input.Email.String()),
		)
		// Don't fail the operation, invitation is created
	}

	s.logger.Info("invitation created",
		zap.String("store_id", input.StoreID.String()),
		zap.String("email", input.Email.String()),
		zap.String("role", input.Role.String()),
	)

	return toInvitationOutput(inv), nil
}

// GetByToken retrieves invitation details by token (for accept page)
func (s *Service) GetByToken(ctx context.Context, token string) (*InvitationDetailsOutput, error) {
	inv, err := s.repo.GetByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	// Use domain method for validation
	if err := inv.CanBeAccepted(); err != nil {
		switch err {
		case domain.ErrInvitationExpired:
			return nil, httpx.ErrGone("invitation has expired")
		case domain.ErrInvitationNotPending:
			return nil, httpx.ErrGone(fmt.Sprintf("invitation is %s", inv.Status().String()))
		default:
			return nil, httpx.ErrGone(err.Error())
		}
	}

	return toInvitationDetailsOutput(inv), nil
}

// List returns all invitations for a store
func (s *Service) List(ctx context.Context, storeID vo.StoreID) ([]InvitationOutput, error) {
	invitations, err := s.repo.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}

	result := make([]InvitationOutput, len(invitations))
	for i, inv := range invitations {
		result[i] = *toInvitationOutput(inv)
	}

	return result, nil
}

// Accept accepts an invitation and adds the user to the store
func (s *Service) Accept(ctx context.Context, input AcceptInvitationInput) (*AcceptInvitationOutput, error) {
	// Get invitation by token
	inv, err := s.repo.GetByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Use domain method to validate acceptance by this email
	if err := inv.CanBeAcceptedBy(input.Email); err != nil {
		switch err {
		case domain.ErrInvitationExpired:
			return nil, httpx.ErrGone("invitation has expired")
		case domain.ErrInvitationNotPending:
			return nil, httpx.ErrGone(fmt.Sprintf("invitation is %s", inv.Status().String()))
		case domain.ErrEmailMismatch:
			return nil, httpx.ErrForbidden("invitation email does not match your account")
		default:
			return nil, httpx.ErrGone(err.Error())
		}
	}

	// Look up internal user ID from Clerk user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, input.ClerkUserID)
	if err != nil {
		s.logger.Error("failed to look up user", zap.Error(err), zap.String("clerk_user_id", input.ClerkUserID))
		return nil, httpx.ErrUnprocessable("user not found - please sync your account first")
	}

	// NEW: Check if user already has a membership (1 user = 1 store rule)
	existingMembership, err := s.membershipLookup.GetMembershipByUserID(ctx, userID)
	if err != nil {
		s.logger.Debug("error checking existing membership", zap.Error(err), zap.String("user_id", userID))
		// Continue - no existing membership found
	}

	if existingMembership != nil {
		// If user is owner of their current store, block acceptance
		if existingMembership.GetRole() == "owner" {
			s.logger.Warn("owner tried to accept invite",
				zap.String("user_id", userID),
				zap.String("current_store", existingMembership.GetStoreName()),
			)
			return nil, httpx.ErrConflict("you are the owner of another store - delete your store first to accept this invitation")
		}

		// User is a member, remove from previous store
		s.logger.Info("removing user from previous store to join new one",
			zap.String("user_id", userID),
			zap.String("old_store", existingMembership.GetStoreName()),
			zap.String("new_store", inv.StoreName()),
		)

		if err := s.membershipLookup.DeleteMembershipByUserID(ctx, userID); err != nil {
			s.logger.Error("failed to remove user from previous store", zap.Error(err))
			return nil, httpx.ErrInternal("failed to leave previous store")
		}
	}

	// Add user to store membership
	err = s.repo.AddUserToStore(ctx, inv.StoreID(), userID, inv.Role(), inv.InvitedBy())
	if err != nil {
		return nil, err
	}

	// Mark invitation as accepted
	inv.Accept()
	err = s.repo.Accept(ctx, inv.ID())
	if err != nil {
		s.logger.Error("failed to mark invitation as accepted",
			zap.Error(err),
			zap.String("invitation_id", inv.ID().String()),
		)
		// Don't fail the operation, user was already added
	}

	s.logger.Info("invitation accepted",
		zap.String("store_id", inv.StoreID().String()),
		zap.String("email", input.Email.String()),
		zap.String("role", inv.Role().String()),
	)

	return &AcceptInvitationOutput{
		StoreID:   inv.StoreID().String(),
		StoreName: inv.StoreName(),
		StoreSlug: inv.StoreSlug(),
		Role:      inv.Role().String(),
	}, nil
}

// Revoke revokes a pending invitation
func (s *Service) Revoke(ctx context.Context, storeID vo.StoreID, invitationID vo.InvitationID) error {
	err := s.repo.Revoke(ctx, storeID, invitationID)
	if err != nil {
		return err
	}

	s.logger.Info("invitation revoked",
		zap.String("store_id", storeID.String()),
		zap.String("invitation_id", invitationID.String()),
	)

	return nil
}

// Resend generates a new token for an existing invitation
func (s *Service) Resend(ctx context.Context, input ResendInvitationInput) (*InvitationOutput, error) {
	// Get existing invitation
	existing, err := s.repo.GetByID(ctx, input.StoreID, input.InvitationID)
	if err != nil {
		return nil, err
	}

	// Use domain method to validate
	if err := existing.CanBeResent(); err != nil {
		return nil, httpx.ErrUnprocessable("can only resend pending invitations")
	}

	// Delete old invitation
	err = s.repo.Delete(ctx, input.StoreID, input.InvitationID)
	if err != nil {
		return nil, err
	}

	// Create new invitation with same email/role
	return s.Create(ctx, CreateInvitationInput{
		StoreID:   input.StoreID,
		InviterID: input.InviterID,
		Email:     existing.Email(),
		Role:      existing.Role(),
	})
}

// ============================================
// Output Converters
// ============================================

func toInvitationOutput(inv *domain.Invitation) *InvitationOutput {
	return &InvitationOutput{
		ID:          inv.ID().String(),
		StoreID:     inv.StoreID().String(),
		Email:       inv.Email().String(),
		Role:        inv.Role().String(),
		Token:       inv.Token().String(),
		Status:      inv.Status().String(),
		InviterName: inv.InviterName(),
		ExpiresAt:   inv.ExpiresAt(),
		AcceptedAt:  inv.AcceptedAt(),
		CreatedAt:   inv.CreatedAt(),
	}
}

func toInvitationDetailsOutput(inv *domain.Invitation) *InvitationDetailsOutput {
	return &InvitationDetailsOutput{
		ID:          inv.ID().String(),
		StoreID:     inv.StoreID().String(),
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
