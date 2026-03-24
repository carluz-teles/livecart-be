package invitation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

const (
	// InvitationExpirationDays is the number of days an invitation is valid
	InvitationExpirationDays = 7
	// TokenLength is the length of the invitation token in bytes (will be hex encoded to 64 chars)
	TokenLength = 32
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
func (s *Service) Create(ctx context.Context, input CreateInvitationInput) (*CreateInvitationOutput, error) {
	// Check if invitation already exists
	existing, err := s.repo.GetByEmail(ctx, input.StoreID, input.Email)
	if err == nil && existing.Status == StatusPending {
		return nil, httpx.ErrConflict("invitation already exists for this email")
	}

	// Generate secure token
	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generating token: %w", err)
	}

	expiresAt := time.Now().Add(time.Hour * 24 * InvitationExpirationDays)

	row, err := s.repo.Create(ctx, input.StoreID, input.Email, input.Role, token, input.InviterID, expiresAt)
	if err != nil {
		return nil, err
	}

	s.logger.Info("invitation created",
		zap.String("store_id", input.StoreID),
		zap.String("email", input.Email),
		zap.String("role", input.Role),
	)

	return &CreateInvitationOutput{
		ID:        row.ID,
		Email:     row.Email,
		Role:      row.Role,
		Token:     row.Token,
		Status:    row.Status,
		ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt,
	}, nil
}

// GetByToken retrieves invitation details by token (for accept page)
func (s *Service) GetByToken(ctx context.Context, token string) (*InvitationDetailsOutput, error) {
	row, err := s.repo.GetByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	// Check if expired
	if row.Status == StatusPending && time.Now().After(row.ExpiresAt) {
		return nil, httpx.ErrGone("invitation has expired")
	}

	// Check if already accepted or revoked
	if row.Status != StatusPending {
		return nil, httpx.ErrGone(fmt.Sprintf("invitation is %s", row.Status))
	}

	return &InvitationDetailsOutput{
		ID:          row.ID,
		StoreID:     row.StoreID,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		StoreName:   row.StoreName,
		StoreSlug:   row.StoreSlug,
		InviterName: row.InviterName,
		ExpiresAt:   row.ExpiresAt,
		CreatedAt:   row.CreatedAt,
	}, nil
}

// List returns all invitations for a store
func (s *Service) List(ctx context.Context, storeID string) ([]InvitationOutput, error) {
	rows, err := s.repo.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}

	result := make([]InvitationOutput, len(rows))
	for i, row := range rows {
		result[i] = InvitationOutput{
			ID:          row.ID,
			StoreID:     row.StoreID,
			Email:       row.Email,
			Role:        row.Role,
			Token:       row.Token,
			Status:      row.Status,
			InviterName: row.InviterName,
			ExpiresAt:   row.ExpiresAt,
			AcceptedAt:  row.AcceptedAt,
			CreatedAt:   row.CreatedAt,
		}
	}

	return result, nil
}

// Accept accepts an invitation and adds the user to the store
func (s *Service) Accept(ctx context.Context, input AcceptInvitationInput) (*AcceptInvitationOutput, error) {
	// Get invitation by token
	row, err := s.repo.GetByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Validate invitation status
	if row.Status != StatusPending {
		return nil, httpx.ErrGone(fmt.Sprintf("invitation is %s", row.Status))
	}

	// Check if expired
	if time.Now().After(row.ExpiresAt) {
		return nil, httpx.ErrGone("invitation has expired")
	}

	// Verify email matches (optional security check)
	if row.Email != input.Email {
		return nil, httpx.ErrForbidden("invitation email does not match your account")
	}

	// Add user to store
	err = s.repo.AddUserToStore(ctx, row.StoreID, input.ClerkUserID, input.Email, input.Name, input.AvatarURL, row.Role, row.InvitedBy)
	if err != nil {
		return nil, err
	}

	// Mark invitation as accepted
	err = s.repo.Accept(ctx, row.ID)
	if err != nil {
		s.logger.Error("failed to mark invitation as accepted",
			zap.Error(err),
			zap.String("invitation_id", row.ID),
		)
		// Don't fail the operation, user was already added
	}

	s.logger.Info("invitation accepted",
		zap.String("store_id", row.StoreID),
		zap.String("email", input.Email),
		zap.String("role", row.Role),
	)

	return &AcceptInvitationOutput{
		StoreID:   row.StoreID,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		Role:      row.Role,
	}, nil
}

// Revoke revokes a pending invitation
func (s *Service) Revoke(ctx context.Context, storeID, invitationID string) error {
	err := s.repo.Revoke(ctx, storeID, invitationID)
	if err != nil {
		return err
	}

	s.logger.Info("invitation revoked",
		zap.String("store_id", storeID),
		zap.String("invitation_id", invitationID),
	)

	return nil
}

// Resend generates a new token for an existing invitation
func (s *Service) Resend(ctx context.Context, storeID, invitationID, inviterID string) (*CreateInvitationOutput, error) {
	// Get existing invitation
	rows, err := s.repo.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}

	var existing *InvitationRow
	for _, row := range rows {
		if row.ID == invitationID {
			existing = &row
			break
		}
	}

	if existing == nil {
		return nil, httpx.ErrNotFound("invitation not found")
	}

	if existing.Status != StatusPending {
		return nil, httpx.ErrUnprocessable("can only resend pending invitations")
	}

	// Delete old invitation
	err = s.repo.Delete(ctx, storeID, invitationID)
	if err != nil {
		return nil, err
	}

	// Create new invitation with same email/role
	return s.Create(ctx, CreateInvitationInput{
		StoreID:   storeID,
		InviterID: inviterID,
		Email:     existing.Email,
		Role:      existing.Role,
	})
}

func generateToken() (string, error) {
	bytes := make([]byte, TokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
