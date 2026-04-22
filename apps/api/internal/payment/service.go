package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("payment"),
	}
}

// Create creates a new payment record
func (s *Service) Create(ctx context.Context, input CreatePaymentInput) (*Payment, error) {
	// Check for idempotency
	if input.IdempotencyKey != nil {
		existing, err := s.repo.GetByIdempotencyKey(ctx, *input.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("checking idempotency: %w", err)
		}
		if existing != nil {
			s.logger.Info("returning existing payment due to idempotency key",
				zap.String("paymentId", existing.ID.String()),
				zap.String("idempotencyKey", *input.IdempotencyKey))
			return existing, nil
		}
	}

	payment, err := s.repo.Create(ctx, input)
	if err != nil {
		return nil, err
	}

	s.logger.Info("payment created",
		zap.String("paymentId", payment.ID.String()),
		zap.String("cartId", payment.CartID.String()),
		zap.String("provider", payment.Provider),
		zap.Int64("amountCents", payment.AmountCents),
		zap.String("status", string(payment.Status)))

	return payment, nil
}

// GetByID retrieves a payment by its ID
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*Payment, error) {
	return s.repo.GetByID(ctx, id)
}

// GetByExternalID retrieves a payment by its external provider ID
func (s *Service) GetByExternalID(ctx context.Context, externalID string) (*Payment, error) {
	return s.repo.GetByExternalID(ctx, externalID)
}

// ListByCart returns all payments for a cart
func (s *Service) ListByCart(ctx context.Context, cartID uuid.UUID) ([]*Payment, error) {
	return s.repo.ListByCart(ctx, cartID)
}

// GetLatestByCart returns the most recent payment for a cart
func (s *Service) GetLatestByCart(ctx context.Context, cartID uuid.UUID) (*Payment, error) {
	return s.repo.GetLatestByCart(ctx, cartID)
}

// UpdateStatus updates the status of a payment
func (s *Service) UpdateStatus(ctx context.Context, id uuid.UUID, input UpdatePaymentStatusInput) error {
	err := s.repo.UpdateStatus(ctx, id, input)
	if err != nil {
		return err
	}

	s.logger.Info("payment status updated",
		zap.String("paymentId", id.String()),
		zap.String("status", string(input.Status)))

	return nil
}

// ProcessWebhook updates a payment based on external webhook notification
// Returns the updated payment for further processing (e.g., cart status update)
func (s *Service) ProcessWebhook(ctx context.Context, input UpdatePaymentByExternalIDInput) (*Payment, error) {
	// Get existing payment
	existing, err := s.repo.GetByExternalID(ctx, input.ExternalPaymentID)
	if err != nil {
		return nil, fmt.Errorf("getting payment: %w", err)
	}
	if existing == nil {
		s.logger.Warn("payment not found for webhook",
			zap.String("externalPaymentId", input.ExternalPaymentID))
		return nil, nil
	}

	// Update payment
	err = s.repo.UpdateByExternalID(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("updating payment: %w", err)
	}

	s.logger.Info("payment updated via webhook",
		zap.String("paymentId", existing.ID.String()),
		zap.String("externalPaymentId", input.ExternalPaymentID),
		zap.String("status", string(input.Status)))

	// Return updated payment
	return s.repo.GetByID(ctx, existing.ID)
}

// MarkAsPaid is a convenience method to mark a payment as approved
func (s *Service) MarkAsPaid(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	return s.UpdateStatus(ctx, id, UpdatePaymentStatusInput{
		Status: PaymentStatusApproved,
		PaidAt: &now,
	})
}

// GetStats returns payment statistics for a store
func (s *Service) GetStats(ctx context.Context, storeID uuid.UUID) (*PaymentStats, error) {
	return s.repo.GetStats(ctx, storeID)
}

// CountByStatus returns payment counts by status for a store
func (s *Service) CountByStatus(ctx context.Context, storeID uuid.UUID) ([]PaymentStatusCount, error) {
	return s.repo.CountByStatus(ctx, storeID)
}
