package invitation

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
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
func (s *Service) Create(ctx context.Context, input CreateInvitationInput) (*domain.Invitation, error) {
	// Check if invitation already exists
	existing, err := s.repo.GetByEmail(ctx, input.StoreID, input.Email)
	if err == nil && existing.IsPending() {
		return nil, httpx.DomainError(409, httpx.CodeInvitationExists, "invitation already exists for this email")
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

	// Group K: member.invited fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberInvited,
		"member.invited:"+inv.ID().String(), struct {
			InvitationID string `json:"invitation_id"`
			StoreID      string `json:"store_id"`
			Role         string `json:"role"`
		}{InvitationID: inv.ID().String(), StoreID: inv.StoreID().String(), Role: inv.Role().String()})

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

	return inv, nil
}

// GetByToken retrieves invitation details by token (for accept page)
func (s *Service) GetByToken(ctx context.Context, token string) (*domain.Invitation, error) {
	inv, err := s.repo.GetByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	// Use domain method for validation
	if err := inv.CanBeAccepted(); err != nil {
		switch err {
		case domain.ErrInvitationExpired:
			return nil, httpx.DomainError(410, httpx.CodeInvitationExpired, "invitation has expired")
		case domain.ErrInvitationNotPending:
			return nil, httpx.DomainError(410, httpx.CodeInvitationNotAcceptable, fmt.Sprintf("invitation is %s", inv.Status().String()))
		default:
			return nil, httpx.ErrGone(err.Error())
		}
	}

	return inv, nil
}

// List returns all invitations for a store as domain entities.
func (s *Service) List(ctx context.Context, storeID vo.StoreID) ([]*domain.Invitation, error) {
	return s.repo.ListByStore(ctx, storeID)
}

// Accept accepts an invitation and adds the user to the store
func (s *Service) Accept(ctx context.Context, input AcceptInvitationInput) (*domain.Invitation, error) {
	// Get invitation by token
	inv, err := s.repo.GetByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Use domain method to validate acceptance by this email
	if err := inv.CanBeAcceptedBy(input.Email); err != nil {
		switch err {
		case domain.ErrInvitationExpired:
			return nil, httpx.DomainError(410, httpx.CodeInvitationExpired, "invitation has expired")
		case domain.ErrInvitationNotPending:
			return nil, httpx.DomainError(410, httpx.CodeInvitationNotAcceptable, fmt.Sprintf("invitation is %s", inv.Status().String()))
		case domain.ErrEmailMismatch:
			return nil, httpx.DomainError(403, httpx.CodeInvitationEmailMismatch, "invitation email does not match your account")
		default:
			return nil, httpx.ErrGone(err.Error())
		}
	}

	// Look up internal user ID from Clerk user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, input.ClerkUserID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to look up user", zap.Error(err), zap.String("clerk_user_id", input.ClerkUserID))
		return nil, httpx.DomainError(422, httpx.CodeUserNotSynced, "user not found - please sync your account first")
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
			return nil, httpx.DomainError(409, httpx.CodeOwnerOfOtherStore, "you are the owner of another store - delete your store first to accept this invitation")
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

	// Group K: member.invite_accepted fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberInviteAccepted,
		"member.invite_accepted:"+inv.ID().String(), struct {
			InvitationID string `json:"invitation_id"`
			StoreID      string `json:"store_id"`
			UserID       string `json:"user_id"`
			Role         string `json:"role"`
		}{InvitationID: inv.ID().String(), StoreID: inv.StoreID().String(), UserID: userID, Role: inv.Role().String()})

	logger.From(ctx, s.logger).Info("invitation accepted",
		zap.String("store_id", inv.StoreID().String()),
		zap.String("email", input.Email.String()),
		zap.String("role", inv.Role().String()),
	)

	return inv, nil
}

// Revoke revokes a pending invitation
func (s *Service) Revoke(ctx context.Context, storeID vo.StoreID, invitationID vo.InvitationID) error {
	err := s.repo.Revoke(ctx, storeID, invitationID)
	if err != nil {
		return err
	}

	// Group K: member.invite_revoked fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberInviteRevoked,
		"member.invite_revoked:"+invitationID.String(), struct {
			InvitationID string `json:"invitation_id"`
			StoreID      string `json:"store_id"`
		}{InvitationID: invitationID.String(), StoreID: storeID.String()})

	logger.From(ctx, s.logger).Info("invitation revoked",
		zap.String("invitation_id", invitationID.String()),
	)

	return nil
}

// Resend generates a new token for an existing invitation
func (s *Service) Resend(ctx context.Context, input ResendInvitationInput) (*domain.Invitation, error) {
	// Get existing invitation
	existing, err := s.repo.GetByID(ctx, input.StoreID, input.InvitationID)
	if err != nil {
		return nil, err
	}

	// Use domain method to validate
	if err := existing.CanBeResent(); err != nil {
		return nil, httpx.DomainError(422, httpx.CodeInvitationNotPending, "can only resend pending invitations")
	}

	// Delete old invitation
	err = s.repo.Delete(ctx, input.StoreID, input.InvitationID)
	if err != nil {
		return nil, err
	}

	// Create new invitation with same email/role
	inv, err := s.Create(ctx, CreateInvitationInput{
		StoreID:   input.StoreID,
		InviterID: input.InviterID,
		Email:     existing.Email(),
		Role:      existing.Role(),
	})
	if err != nil {
		return nil, err
	}

	// Group K: member.invite_resent fact (best-effort, observability only).
	// Keyed on the OLD invitation id — the resend is one logical action even
	// though Create mints a fresh row/token.
	_ = events.EmitInternal(ctx, s.repo.q, events.MemberInviteResent,
		"member.invite_resent:"+input.InvitationID.String(), struct {
			InvitationID    string `json:"invitation_id"`
			OldInvitationID string `json:"old_invitation_id"`
			StoreID         string `json:"store_id"`
		}{InvitationID: inv.ID().String(), OldInvitationID: input.InvitationID.String(), StoreID: input.StoreID.String()})

	return inv, nil
}
