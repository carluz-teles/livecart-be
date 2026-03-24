package invitation

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("invitation"),
	}
}

// Create creates a new invitation for a user to join a store
func (s *Service) Create(ctx context.Context, input CreateInvitationInput) (*InvitationOutput, error) {
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
