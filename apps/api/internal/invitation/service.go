package invitation

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/internal/store"
	"livecart/apps/api/lib/clerk"
	"livecart/apps/api/lib/config"
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
		logger:    logger.Named("invitation"),
	}
}

// Create creates a new invitation for a user to join a store
// If Clerk SDK is available and store has clerk_org_id, uses Clerk (emails sent automatically)
// Otherwise falls back to local database (backward compatibility)
func (s *Service) Create(ctx context.Context, input CreateInvitationInput) (*InvitationOutput, error) {
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, input.StoreID.String())
	if err != nil {
		return nil, fmt.Errorf("getting store: %w", err)
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		return s.createViaClerk(ctx, input, storeData.ClerkOrgID)
	}

	// Fallback to local database
	return s.createLocal(ctx, input)
}

// createViaClerk creates invitation using Clerk SDK (email sent automatically)
func (s *Service) createViaClerk(ctx context.Context, input CreateInvitationInput, clerkOrgID string) (*InvitationOutput, error) {
	// Convert role to Clerk format
	clerkRole := "org:member"
	if input.Role.String() == "admin" {
		clerkRole = "org:admin"
	}

	// Build redirect URL for accept page
	redirectURL := fmt.Sprintf("%s/accept-invite", config.FrontendURL.StringOr("http://localhost:3000"))

	// Create invitation via Clerk SDK (email sent automatically!)
	inv, err := s.clerkSDK.InviteMember(ctx,
		clerkOrgID,
		input.Email.String(),
		clerkRole,
		"", // inviterUserID - we don't have the Clerk user ID here
		redirectURL,
	)
	if err != nil {
		s.logger.Error("failed to create clerk invitation",
			zap.Error(err),
			zap.String("org_id", clerkOrgID),
			zap.String("email", input.Email.String()),
		)
		// Fallback to local if Clerk fails
		return s.createLocal(ctx, input)
	}

	s.logger.Info("invitation created via Clerk",
		zap.String("clerk_org_id", clerkOrgID),
		zap.String("email", input.Email.String()),
		zap.String("role", clerkRole),
		zap.String("invitation_id", inv.ID),
	)

	return &InvitationOutput{
		ID:        inv.ID,
		StoreID:   input.StoreID.String(),
		Email:     inv.EmailAddress,
		Role:      input.Role.String(),
		Token:     "", // Clerk manages tokens
		Status:    inv.Status,
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour), // Default Clerk expiration
		CreatedAt: time.Unix(inv.CreatedAt, 0),
	}, nil
}

// createLocal creates invitation in local database (legacy flow)
func (s *Service) createLocal(ctx context.Context, input CreateInvitationInput) (*InvitationOutput, error) {
	// Check if invitation already exists
	existing, err := s.repo.GetByEmail(ctx, input.StoreID, input.Email)
	if err == nil && existing.IsPending() {
		return nil, httpx.ErrConflict("invitation already exists for this email")
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

	s.logger.Info("invitation created (local)",
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
// If store has clerk_org_id, fetches from Clerk; otherwise from local database
func (s *Service) List(ctx context.Context, storeID vo.StoreID) ([]InvitationOutput, error) {
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, storeID.String())
	if err != nil {
		return nil, fmt.Errorf("getting store: %w", err)
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		invList, err := s.clerkSDK.ListInvitations(ctx, storeData.ClerkOrgID)
		if err != nil {
			s.logger.Error("failed to list clerk invitations, falling back to local",
				zap.Error(err),
				zap.String("org_id", storeData.ClerkOrgID),
			)
			// Fallback to local
			return s.listLocal(ctx, storeID)
		}

		result := make([]InvitationOutput, len(invList.OrganizationInvitations))
		for i, inv := range invList.OrganizationInvitations {
			role := "member"
			if inv.Role == "org:admin" {
				role = "admin"
			}
			result[i] = InvitationOutput{
				ID:        inv.ID,
				StoreID:   storeID.String(),
				Email:     inv.EmailAddress,
				Role:      role,
				Status:    inv.Status,
				ExpiresAt: time.Now().Add(7 * 24 * time.Hour), // Clerk default
				CreatedAt: time.Unix(inv.CreatedAt, 0),
			}
		}
		return result, nil
	}

	// Fallback to local database
	return s.listLocal(ctx, storeID)
}

// listLocal lists invitations from local database
func (s *Service) listLocal(ctx context.Context, storeID vo.StoreID) ([]InvitationOutput, error) {
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

	// Add user to store
	err = s.repo.AddUserToStore(ctx, inv.StoreID(), input.ClerkUserID, input.Email, input.Name, input.AvatarURL, inv.Role(), inv.InvitedBy())
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
// If store has clerk_org_id, revokes via Clerk; otherwise via local database
func (s *Service) Revoke(ctx context.Context, storeID vo.StoreID, invitationID vo.InvitationID) error {
	// Get store to check for clerk_org_id
	storeData, err := s.storeRepo.GetByID(ctx, storeID.String())
	if err != nil {
		return fmt.Errorf("getting store: %w", err)
	}

	// If store has clerk_org_id and we have Clerk SDK, use Clerk
	if storeData.ClerkOrgID != "" && s.clerkSDK != nil {
		_, err := s.clerkSDK.RevokeInvitation(ctx, storeData.ClerkOrgID, invitationID.String())
		if err != nil {
			s.logger.Error("failed to revoke clerk invitation, trying local",
				zap.Error(err),
				zap.String("org_id", storeData.ClerkOrgID),
				zap.String("invitation_id", invitationID.String()),
			)
			// Try local as fallback (might be an old invitation)
		} else {
			s.logger.Info("invitation revoked via Clerk",
				zap.String("clerk_org_id", storeData.ClerkOrgID),
				zap.String("invitation_id", invitationID.String()),
			)
			return nil
		}
	}

	// Revoke from local database
	err = s.repo.Revoke(ctx, storeID, invitationID)
	if err != nil {
		return err
	}

	s.logger.Info("invitation revoked (local)",
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
