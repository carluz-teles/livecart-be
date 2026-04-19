package store

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// MembershipCreator interface to avoid circular dependency with user package
type MembershipCreator interface {
	CreateOwnerMembership(ctx context.Context, storeID, userID string) (membershipID string, err error)
	// HasMembership checks if user already has a membership (1 user = 1 store)
	HasMembership(ctx context.Context, userID string) (bool, error)
}

// UserLookup interface to look up users by Clerk ID
type UserLookup interface {
	GetUserIDByClerkID(ctx context.Context, clerkUserID string) (userID string, err error)
}

type Service struct {
	repo              *Repository
	membershipCreator MembershipCreator
	userLookup        UserLookup
	logger            *zap.Logger
}

func NewService(repo *Repository, membershipCreator MembershipCreator, userLookup UserLookup, logger *zap.Logger) *Service {
	return &Service{
		repo:              repo,
		membershipCreator: membershipCreator,
		userLookup:        userLookup,
		logger:            logger.Named("store"),
	}
}

// Create creates a new store and owner membership
func (s *Service) Create(ctx context.Context, input CreateStoreInput) (CreateStoreOutput, error) {
	// 1. Look up internal user ID from Clerk user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, input.ClerkUserID)
	if err != nil {
		s.logger.Error("failed to look up user", zap.Error(err), zap.String("clerk_user_id", input.ClerkUserID))
		return CreateStoreOutput{}, httpx.ErrUnprocessable("user not found - please sync your account first")
	}

	// 2. Check if user already has a store (1 user = 1 store rule)
	hasMembership, err := s.membershipCreator.HasMembership(ctx, userID)
	if err != nil {
		s.logger.Error("failed to check existing membership", zap.Error(err), zap.String("user_id", userID))
		return CreateStoreOutput{}, fmt.Errorf("checking existing membership: %w", err)
	}
	if hasMembership {
		return CreateStoreOutput{}, httpx.ErrConflict("you already have a store - delete your current store first to create a new one")
	}

	// 3. Check slug uniqueness
	existing, err := s.repo.GetBySlug(ctx, input.Slug)
	if err != nil && !httpx.IsNotFound(err) {
		return CreateStoreOutput{}, fmt.Errorf("checking slug uniqueness: %w", err)
	}
	if existing != nil {
		return CreateStoreOutput{}, httpx.ErrConflict("slug already in use")
	}

	// 4. Create store
	storeRow, err := s.repo.Create(ctx, CreateStoreParams{
		Name: input.Name,
		Slug: input.Slug,
	})
	if err != nil {
		s.logger.Error("failed to create store", zap.Error(err), zap.String("slug", input.Slug))
		return CreateStoreOutput{}, fmt.Errorf("creating store: %w", err)
	}

	// 5. Create owner membership
	membershipID, err := s.membershipCreator.CreateOwnerMembership(ctx, storeRow.ID, userID)
	if err != nil {
		s.logger.Error("failed to create owner membership", zap.Error(err), zap.String("store_id", storeRow.ID))
		return CreateStoreOutput{}, fmt.Errorf("creating owner membership: %w", err)
	}

	s.logger.Info("store created successfully",
		zap.String("store_id", storeRow.ID),
		zap.String("membership_id", membershipID),
		zap.String("user_id", userID),
	)

	return CreateStoreOutput{
		ID:           storeRow.ID,
		Name:         storeRow.Name,
		Slug:         storeRow.Slug,
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
		Description:    input.Description,
		Website:        input.Website,
		LogoURL:        input.LogoURL,
		AddressStreet:  input.Address.Street,
		AddressCity:    input.Address.City,
		AddressState:   input.Address.State,
		AddressZip:     input.Address.Zip,
		AddressCountry: input.Address.Country,
	})
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func (s *Service) UpdateCartSettings(ctx context.Context, input UpdateCartSettingsInput) (StoreOutput, error) {
	row, err := s.repo.UpdateCartSettings(ctx, UpdateCartSettingsParams{
		ID:                        input.StoreID,
		Enabled:                   input.Enabled,
		ExpirationMinutes:         input.ExpirationMinutes,
		ReserveStock:              input.ReserveStock,
		MaxItems:                  input.MaxItems,
		MaxQuantityPerItem:        input.MaxQuantityPerItem,
		NotifyBeforeExpiration:    input.NotifyBeforeExpiration,
		AllowEdit:                 input.AllowEdit,
		AutoSendCheckoutLinks:     input.AutoSendCheckoutLinks,
		CheckoutLinkExpiryHours:   input.CheckoutLinkExpiryHours,
		CheckoutSendMethods:       input.CheckoutSendMethods,
		SendOnFirstItem:           input.SendOnFirstItem,
		SendOnNewItems:            input.SendOnNewItems,
		MessageCooldownSeconds:    input.MessageCooldownSeconds,
		SendExpirationReminder:    input.SendExpirationReminder,
		ExpirationReminderMinutes: input.ExpirationReminderMinutes,
	})
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func (s *Service) UpdateLogoURL(ctx context.Context, storeID string, logoURL string) (StoreOutput, error) {
	row, err := s.repo.UpdateLogoURL(ctx, storeID, logoURL)
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func (s *Service) GetByClerkUserID(ctx context.Context, clerkUserID string) (StoreOutput, error) {
	// Look up internal user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, clerkUserID)
	if err != nil {
		return StoreOutput{}, httpx.ErrNotFound("user not found")
	}

	row, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(*row), nil
}

func toStoreOutput(row StoreRow) StoreOutput {
	var address *AddressDTO
	if row.AddressStreet != nil || row.AddressCity != nil || row.AddressState != nil || row.AddressZip != nil || row.AddressCountry != nil {
		address = &AddressDTO{
			Street:  deref(row.AddressStreet),
			City:    deref(row.AddressCity),
			State:   deref(row.AddressState),
			Zip:     deref(row.AddressZip),
			Country: deref(row.AddressCountry),
		}
	}

	return StoreOutput{
		ID:             row.ID,
		Name:           row.Name,
		Slug:           row.Slug,
		Active:         row.Active,
		WhatsappNumber: row.WhatsappNumber,
		EmailAddress:   row.EmailAddress,
		SMSNumber:      row.SMSNumber,
		Description:    row.Description,
		Website:        row.Website,
		LogoURL:        row.LogoURL,
		Address:        address,
		CartSettings:   row.CartSettings,
		CreatedAt:      row.CreatedAt,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// StoreLookupAdapter implements invitation.StoreLookup interface
type StoreLookupAdapter struct {
	service *Service
}

// NewStoreLookupAdapter creates a new adapter for store lookup
func NewStoreLookupAdapter(service *Service) *StoreLookupAdapter {
	return &StoreLookupAdapter{service: service}
}

// GetStoreNameByID implements invitation.StoreLookup
func (a *StoreLookupAdapter) GetStoreNameByID(ctx context.Context, storeID string) (string, error) {
	store, err := a.service.GetByID(ctx, storeID)
	if err != nil {
		return "", err
	}
	return store.Name, nil
}
