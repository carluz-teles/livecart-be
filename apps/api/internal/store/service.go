package store

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/clerk"
	"livecart/apps/api/lib/httpx"
)

// MembershipCreator interface to avoid circular dependency with user package
type MembershipCreator interface {
	CreateOwnerMembership(ctx context.Context, storeID, clerkUserID, email, name, avatarURL string) (membershipID string, err error)
}

type Service struct {
	repo              *Repository
	clerkSDK          *clerk.SDK
	membershipCreator MembershipCreator
	logger            *zap.Logger
}

func NewService(repo *Repository, clerkSDK *clerk.SDK, membershipCreator MembershipCreator, logger *zap.Logger) *Service {
	return &Service{
		repo:              repo,
		clerkSDK:          clerkSDK,
		membershipCreator: membershipCreator,
		logger:            logger.Named("store"),
	}
}

// Create creates a new store with Clerk organization and owner membership
func (s *Service) Create(ctx context.Context, input CreateStoreInput) (CreateStoreOutput, error) {
	// 1. Check slug uniqueness
	existing, err := s.repo.GetBySlug(ctx, input.Slug)
	if err != nil && !httpx.IsNotFound(err) {
		return CreateStoreOutput{}, fmt.Errorf("checking slug uniqueness: %w", err)
	}
	if existing != nil {
		return CreateStoreOutput{}, httpx.ErrConflict("slug already in use")
	}

	// 2. Create Clerk organization
	clerkOrg, err := s.clerkSDK.CreateOrganization(ctx, input.Name, input.Slug, input.ClerkUserID)
	if err != nil {
		s.logger.Error("failed to create clerk organization", zap.Error(err), zap.String("slug", input.Slug))
		return CreateStoreOutput{}, fmt.Errorf("creating clerk organization: %w", err)
	}

	// 3. Create store with clerk_org_id
	storeRow, err := s.repo.Create(ctx, CreateStoreParams{
		Name:       input.Name,
		Slug:       input.Slug,
		ClerkOrgID: clerkOrg.ID,
	})
	if err != nil {
		// TODO: Consider rolling back Clerk org creation
		s.logger.Error("failed to create store", zap.Error(err), zap.String("clerk_org_id", clerkOrg.ID))
		return CreateStoreOutput{}, fmt.Errorf("creating store: %w", err)
	}

	// 4. Create owner membership
	membershipID, err := s.membershipCreator.CreateOwnerMembership(
		ctx,
		storeRow.ID,
		input.ClerkUserID,
		input.Email,
		input.UserName,
		input.AvatarURL,
	)
	if err != nil {
		// TODO: Consider rolling back store creation
		s.logger.Error("failed to create owner membership", zap.Error(err), zap.String("store_id", storeRow.ID))
		return CreateStoreOutput{}, fmt.Errorf("creating owner membership: %w", err)
	}

	s.logger.Info("store created successfully",
		zap.String("store_id", storeRow.ID),
		zap.String("clerk_org_id", clerkOrg.ID),
		zap.String("membership_id", membershipID),
	)

	return CreateStoreOutput{
		ID:           storeRow.ID,
		Name:         storeRow.Name,
		Slug:         storeRow.Slug,
		ClerkOrgID:   clerkOrg.ID,
		MembershipID: membershipID,
		CreatedAt:    storeRow.CreatedAt,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id string) (StoreOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(*row), nil
}

func (s *Service) Update(ctx context.Context, input UpdateStoreInput) (StoreOutput, error) {
	row, err := s.repo.Update(ctx, UpdateStoreParams{
		ID:             input.StoreID,
		Name:           input.Name,
		WhatsappNumber: input.WhatsappNumber,
		EmailAddress:   input.EmailAddress,
		SMSNumber:      input.SMSNumber,
	})
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func (s *Service) UpdateCartSettings(ctx context.Context, input UpdateCartSettingsInput) (StoreOutput, error) {
	row, err := s.repo.UpdateCartSettings(ctx, UpdateCartSettingsParams{
		ID:                     input.StoreID,
		Enabled:                input.Enabled,
		ExpirationMinutes:      input.ExpirationMinutes,
		ReserveStock:           input.ReserveStock,
		MaxItems:               input.MaxItems,
		MaxQuantityPerItem:     input.MaxQuantityPerItem,
		NotifyBeforeExpiration: input.NotifyBeforeExpiration,
	})
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func (s *Service) GetByClerkUserID(ctx context.Context, clerkUserID string) (StoreOutput, error) {
	row, err := s.repo.GetByClerkUserID(ctx, clerkUserID)
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(*row), nil
}

func toStoreOutput(row StoreRow) StoreOutput {
	return StoreOutput{
		ID:             row.ID,
		Name:           row.Name,
		Slug:           row.Slug,
		Active:         row.Active,
		WhatsappNumber: row.WhatsappNumber,
		EmailAddress:   row.EmailAddress,
		SMSNumber:      row.SMSNumber,
		CartSettings:   row.CartSettings,
		CreatedAt:      row.CreatedAt,
	}
}
