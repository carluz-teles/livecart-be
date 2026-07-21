package invitation

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/email"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
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
		logger.From(ctx, s.logger).Warn("could not look up store name", zap.Error(err))
		storeName = "Store" // Fallback
	}

	inviterName, err := s.memberLookup.GetMemberNameByID(ctx, input.StoreID.String(), input.InviterID.String())
	if err != nil {
		logger.From(ctx, s.logger).Warn("could not look up inviter name", zap.Error(err))
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
		StoreID:     input.StoreID.String(),
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to send invitation email",
			zap.Error(err),
			zap.String("email", input.Email.String()),
		)
		// Don't fail the operation, invitation is created
	}

	logger.From(ctx, s.logger).Info("invitation created",
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
		logger.From(ctx, s.logger).Error("failed to look up user", zap.Error(err), zap.String("clerk_user_id", input.ClerkUserID))
		return nil, httpx.ErrUnprocessable("user not found - please sync your account first")
	}

	// Check if user already has a membership (1 user = 1 store rule).
	// FAIL-CLOSED: erro real na consulta aborta o aceite — continuar aqui
	// pulava a checagem de dono e criava membership indevida.
	existingMembership, err := s.membershipLookup.GetMembershipByUserID(ctx, userID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to check existing membership", zap.Error(err), zap.String("user_id", userID))
		return nil, httpx.ErrInternal("failed to verify your current store membership")
	}

	if existingMembership != nil {
		// If user is owner of their current store, block acceptance
		if existingMembership.GetRole() == "owner" {
			logger.From(ctx, s.logger).Warn("owner tried to accept invite",
				zap.String("user_id", userID),
				zap.String("current_store", existingMembership.GetStoreName()),
			)
			return nil, httpx.ErrConflict("you are the owner of another store - delete your store first to accept this invitation")
		}

		// User is a member of another store — the swap happens inside the
		// atomic accept below
		logger.From(ctx, s.logger).Info("user will be moved from previous store",
			zap.String("user_id", userID),
			zap.String("old_store", existingMembership.GetStoreName()),
			zap.String("new_store", inv.StoreName()),
		)
	}

	// Troca de loja + criação da membership + marcação do convite numa única
	// transação — nunca mais "membership criada com convite pendente".
	inv.Accept()
	if err := s.repo.AcceptAtomically(ctx, inv.ID(), inv.StoreID(), userID, inv.Role(), inv.InvitedBy(), existingMembership != nil); err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("invitation accepted",
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

	logger.From(ctx, s.logger).Info("invitation revoked",
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
