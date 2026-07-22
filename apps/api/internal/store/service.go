package store

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/billing"
	"livecart/apps/api/internal/events"
	storedomain "livecart/apps/api/internal/store/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
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
	billing           *billing.Service // optional — starts the 7-day trial (PRD 007)
	logger            *zap.Logger
}

// SetBilling wires the billing service (optional; store creation degrades to
// no-trial when absent, and /users/sync lazily ensures it later).
func (s *Service) SetBilling(b *billing.Service) {
	s.billing = b
}

func NewService(repo *Repository, membershipCreator MembershipCreator, userLookup UserLookup, logger *zap.Logger) *Service {
	return &Service{
		repo:              repo,
		membershipCreator: membershipCreator,
		userLookup:        userLookup,
		logger:            logger.Named("store"),
	}
}

// Create creates a new store and owner membership, returning the domain entity.
func (s *Service) Create(ctx context.Context, input CreateStoreInput) (*storedomain.Store, error) {
	// 1. Look up internal user ID from Clerk user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, input.ClerkUserID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to look up user", zap.Error(err), zap.String("clerk_user_id", input.ClerkUserID))
		return nil, httpx.ErrUnprocessable("user not found - please sync your account first")
	}

	// 2. Check if user already has a store (1 user = 1 store rule)
	hasMembership, err := s.membershipCreator.HasMembership(ctx, userID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to check existing membership", zap.Error(err), zap.String("user_id", userID))
		return nil, fmt.Errorf("checking existing membership: %w", err)
	}
	if hasMembership {
		return nil, httpx.ErrConflict("you already have a store - delete your current store first to create a new one")
	}

	// 3. Check slug uniqueness
	existing, err := s.repo.GetBySlug(ctx, input.Slug)
	if err != nil && !httpx.IsNotFound(err) {
		return nil, fmt.Errorf("checking slug uniqueness: %w", err)
	}
	if existing != nil {
		return nil, httpx.ErrConflict("slug already in use")
	}

	// 4. Create store
	store, err := s.repo.Create(ctx, CreateStoreParams{
		Name: input.Name,
		Slug: input.Slug,
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create store", zap.Error(err), zap.String("slug", input.Slug))
		return nil, fmt.Errorf("creating store: %w", err)
	}
	storeID := store.ID().String()

	// 5. Create owner membership
	membershipID, err := s.membershipCreator.CreateOwnerMembership(ctx, storeID, userID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create owner membership", zap.Error(err), zap.String("store_id", storeID))
		return nil, fmt.Errorf("creating owner membership: %w", err)
	}

	// Group K: store.created fact (best-effort, observability only).
	_ = events.EmitInternal(ctx, s.repo.q, events.StoreCreated, "store.created:"+storeID, struct {
		StoreID string `json:"store_id"`
		UserID  string `json:"user_id"`
		Slug    string `json:"slug"`
	}{StoreID: storeID, UserID: userID, Slug: store.Slug().String()})

	// 6. Start the 7-day cardless trial (PRD 007). Non-fatal: /users/sync
	// lazily retries, so a Stripe hiccup never blocks onboarding.
	if s.billing != nil {
		if _, err := s.billing.EnsureTrialSubscription(ctx, storeID, store.Name(), ""); err != nil {
			logger.From(ctx, s.logger).Warn("trial provisioning failed at store creation",
				zap.String("store_id", storeID),
				zap.Error(err),
			)
		}
	}

	logger.From(ctx, s.logger).Info("store created successfully",
		zap.String("store_id", storeID),
		zap.String("membership_id", membershipID),
		zap.String("user_id", userID),
	)

	return store, nil
}

func (s *Service) GetByID(ctx context.Context, id string) (*storedomain.Store, error) {
	return s.repo.GetByID(ctx, id)
}

// Update merges the incoming request with the store's current persisted values
// (subset semantics), normalizes CEP/UF/CNPJ and persists the result.
func (s *Service) Update(ctx context.Context, input UpdateStoreInput) (*storedomain.Store, error) {
	current, err := s.repo.GetByID(ctx, input.StoreID)
	if err != nil {
		return nil, err
	}

	merged, err := normalizeUpdateStoreInput(current, input)
	if err != nil {
		return nil, httpx.ErrBadRequest(err.Error())
	}

	updated, err := s.repo.Update(ctx, UpdateStoreParams{
		ID:                   input.StoreID,
		Name:                 merged.Name,
		WhatsappNumber:       merged.WhatsappNumber,
		EmailAddress:         merged.EmailAddress,
		SMSNumber:            merged.SMSNumber,
		Description:          merged.Description,
		Website:              merged.Website,
		LogoURL:              merged.LogoURL,
		AddressStreet:        merged.Address.Street,
		AddressNumber:        merged.Address.Number,
		AddressComplement:    merged.Address.Complement,
		AddressDistrict:      merged.Address.District,
		AddressCity:          merged.Address.City,
		AddressState:         merged.Address.State,
		AddressZip:           merged.Address.Zip,
		AddressCountry:       merged.Address.Country,
		AddressStateRegister: merged.Address.StateRegister,
		CNPJ:                 merged.CNPJ,
	})
	if err != nil {
		return nil, err
	}
	s.emitSettingsUpdated(ctx, input.StoreID, "profile")
	return updated, nil
}

// emitSettingsUpdated fires the Group K store.settings_updated fact
// (best-effort, observability only). The dedup key includes the kind so
// distinct update paths within one store are not collapsed together.
func (s *Service) emitSettingsUpdated(ctx context.Context, storeID, kind string) {
	_ = events.EmitInternal(ctx, s.repo.q, events.StoreSettingsUpdated,
		"store.settings_updated:"+storeID+":"+kind, struct {
			StoreID string `json:"store_id"`
			Kind    string `json:"kind"`
		}{StoreID: storeID, Kind: kind})
}

func (s *Service) UpdateShippingDefaults(ctx context.Context, input UpdateShippingDefaultsInput) (*storedomain.Store, error) {
	format := input.PackageFormat
	if format == "" {
		format = "box"
	}
	// Default dimensions are all-or-nothing: if any of the three is missing,
	// the fallback is disabled and we persist NULLs across the board.
	hCm, wCm, lCm := input.HeightCm, input.WidthCm, input.LengthCm
	if hCm == nil || wCm == nil || lCm == nil {
		hCm, wCm, lCm = nil, nil, nil
	}
	updated, err := s.repo.UpdateShippingDefaults(ctx, UpdateShippingDefaultsParams{
		ID:                 input.StoreID,
		PackageWeightGrams: input.PackageWeightGrams,
		PackageFormat:      format,
		HeightCm:           hCm,
		WidthCm:            wCm,
		LengthCm:           lCm,
	})
	if err != nil {
		return nil, err
	}
	s.emitSettingsUpdated(ctx, input.StoreID, "shipping")
	return updated, nil
}

func (s *Service) UpdateCartSettings(ctx context.Context, input UpdateCartSettingsInput) (*storedomain.Store, error) {
	updated, err := s.repo.UpdateCartSettings(ctx, UpdateCartSettingsParams{
		ID:                        input.StoreID,
		Enabled:                   input.Enabled,
		ExpirationMinutes:         input.ExpirationMinutes,
		ReserveStock:              input.ReserveStock,
		AllowStorePickup:          input.AllowStorePickup,
		MaxQuantityPerItem:        input.MaxQuantityPerItem,
		AllowEdit:                 input.AllowEdit,
		CheckoutSendMethods:       input.CheckoutSendMethods,
		RealTimeCart:              input.RealTimeCart,
		SendOnLiveEnd:             input.SendOnLiveEnd,
		MessageCooldownSeconds:    input.MessageCooldownSeconds,
		SendExpirationReminder:    input.SendExpirationReminder,
		ExpirationReminderMinutes: input.ExpirationReminderMinutes,
	})
	if err != nil {
		return nil, err
	}
	s.emitSettingsUpdated(ctx, input.StoreID, "cart_settings")
	return updated, nil
}

func (s *Service) UpdateLogoURL(ctx context.Context, storeID string, logoURL string) (*storedomain.Store, error) {
	updated, err := s.repo.UpdateLogoURL(ctx, storeID, logoURL)
	if err != nil {
		return nil, err
	}
	s.emitSettingsUpdated(ctx, storeID, "logo")
	return updated, nil
}

func (s *Service) GetByClerkUserID(ctx context.Context, clerkUserID string) (*storedomain.Store, error) {
	// Look up internal user ID
	userID, err := s.userLookup.GetUserIDByClerkID(ctx, clerkUserID)
	if err != nil {
		return nil, httpx.ErrNotFound("user not found")
	}

	return s.repo.GetByUserID(ctx, userID)
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
	return store.Name(), nil
}
