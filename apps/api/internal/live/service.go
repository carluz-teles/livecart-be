package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"go.uber.org/zap"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("live"),
	}
}

func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	row, err := s.repo.Create(ctx, CreateLiveParams{
		StoreID:        input.StoreID,
		Title:          input.Title,
		Platform:       input.Platform,
		PlatformLiveID: input.PlatformLiveID,
		Status:         "scheduled",
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	return CreateLiveOutput{
		ID:        row.ID,
		Title:     row.Title,
		Platform:  row.Platform,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (LiveOutput, error) {
	row, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	return toLiveOutput(*row), nil
}

func (s *Service) List(ctx context.Context, input ListLivesInput) (ListLivesOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListLivesParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListLivesOutput{}, err
	}

	lives := make([]LiveOutput, len(result.Lives))
	for i, row := range result.Lives {
		lives[i] = toLiveOutput(row)
	}

	return ListLivesOutput{
		Lives:      lives,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateLiveInput) (LiveOutput, error) {
	row, err := s.repo.Update(ctx, UpdateLiveParams{
		ID:             input.ID,
		StoreID:        input.StoreID,
		Title:          input.Title,
		Platform:       input.Platform,
		PlatformLiveID: input.PlatformLiveID,
	})
	if err != nil {
		return LiveOutput{}, err
	}

	return toLiveOutput(row), nil
}

func (s *Service) Start(ctx context.Context, id, storeID string) (LiveOutput, error) {
	row, err := s.repo.Start(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}
	return toLiveOutput(row), nil
}

func (s *Service) End(ctx context.Context, input EndLiveInput) (EndLiveOutput, error) {
	// 1. End the live session
	row, err := s.repo.End(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}

	// 2. Finalize all pending carts (mark as 'checkout')
	cartsFinalized, err := s.repo.FinalizeCartsBySession(ctx, input.ID)
	if err != nil {
		s.logger.Error("failed to finalize carts",
			zap.String("session_id", input.ID),
			zap.Error(err),
		)
		// Don't fail the whole operation, just log
	}

	// 3. Determine if we should auto-send checkout links
	autoSend := false
	if input.AutoSend != nil {
		// Use override value
		autoSend = *input.AutoSend
	} else {
		// Use store default
		storeDefault, err := s.repo.GetStoreAutoSendSetting(ctx, input.StoreID)
		if err != nil {
			s.logger.Error("failed to get store auto_send setting",
				zap.String("store_id", input.StoreID),
				zap.Error(err),
			)
		} else {
			autoSend = storeDefault
		}
	}

	s.logger.Info("live session ended",
		zap.String("session_id", input.ID),
		zap.Int("carts_finalized", cartsFinalized),
		zap.Bool("auto_send_links", autoSend),
	)

	// 4. TODO: If autoSend, trigger notification job (future)
	// if autoSend && cartsFinalized > 0 {
	//     s.notificationService.SendCheckoutLinks(ctx, input.ID)
	// }

	return EndLiveOutput{
		Live:           toLiveOutput(row),
		CartsFinalized: cartsFinalized,
		AutoSendLinks:  autoSend,
	}, nil
}

func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.Delete(ctx, id, storeID)
}

func (s *Service) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

// =============================================================================
// CART OPERATIONS
// =============================================================================

// AddToCart adds a product to a user's cart during a live session.
// Creates a new cart if one doesn't exist for this user in this session.
func (s *Service) AddToCart(ctx context.Context, input AddToCartInput) (AddToCartOutput, error) {
	// Generate token for new carts
	token, err := generateCartToken()
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("generating cart token: %w", err)
	}

	// Get or create cart for this user in this session
	cart, isNew, err := s.repo.GetOrCreateCart(ctx, GetOrCreateCartParams{
		SessionID:      input.SessionID,
		PlatformUserID: input.PlatformUserID,
		PlatformHandle: input.PlatformHandle,
		Token:          token,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("getting or creating cart: %w", err)
	}

	// Add item to cart
	err = s.repo.AddCartItem(ctx, AddCartItemParams{
		CartID:    cart.ID,
		ProductID: input.ProductID,
		Quantity:  input.Quantity,
		UnitPrice: input.ProductPrice,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("adding item to cart: %w", err)
	}

	s.logger.Info("added product to cart",
		zap.String("cart_id", cart.ID),
		zap.String("product_id", input.ProductID),
		zap.Int("quantity", input.Quantity),
		zap.Bool("new_cart", isNew),
	)

	return AddToCartOutput{
		CartID:    cart.ID,
		IsNewCart: isNew,
	}, nil
}

// generateCartToken creates a random token for cart checkout URLs.
func generateCartToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// =============================================================================
// PLATFORM AGGREGATION
// =============================================================================

// AddPlatform adds a platform ID to a live session.
func (s *Service) AddPlatform(ctx context.Context, input AddPlatformInput) (AddPlatformOutput, error) {
	// Verify session exists and belongs to store
	_, err := s.repo.GetByID(ctx, input.SessionID, input.StoreID)
	if err != nil {
		return AddPlatformOutput{}, err
	}

	row, err := s.repo.AddPlatformToSession(ctx, input.SessionID, input.Platform, input.PlatformLiveID)
	if err != nil {
		return AddPlatformOutput{}, err
	}

	s.logger.Info("platform added to session",
		zap.String("session_id", input.SessionID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return AddPlatformOutput{
		ID:             row.ID,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt,
	}, nil
}

// ListPlatforms returns all platforms associated with a session.
func (s *Service) ListPlatforms(ctx context.Context, sessionID, storeID string) ([]AddPlatformOutput, error) {
	// Verify session exists and belongs to store
	_, err := s.repo.GetByID(ctx, sessionID, storeID)
	if err != nil {
		return nil, err
	}

	rows, err := s.repo.ListPlatformsBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	platforms := make([]AddPlatformOutput, len(rows))
	for i, row := range rows {
		platforms[i] = AddPlatformOutput{
			ID:             row.ID,
			Platform:       row.Platform,
			PlatformLiveID: row.PlatformLiveID,
			AddedAt:        row.AddedAt,
		}
	}

	return platforms, nil
}

// RemovePlatform removes a platform ID from a session.
func (s *Service) RemovePlatform(ctx context.Context, sessionID, storeID, platformLiveID string) error {
	// Verify session exists and belongs to store
	_, err := s.repo.GetByID(ctx, sessionID, storeID)
	if err != nil {
		return err
	}

	return s.repo.RemovePlatformFromSession(ctx, sessionID, platformLiveID)
}

// GetSessionByPlatformLiveID finds an active session by any associated platform_live_id.
func (s *Service) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*LiveOutput, error) {
	row, err := s.repo.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}

	output := toLiveOutput(*row)
	return &output, nil
}

func toLiveOutput(row LiveRow) LiveOutput {
	return LiveOutput{
		ID:             row.ID,
		StoreID:        row.StoreID,
		Title:          row.Title,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		Status:         row.Status,
		StartedAt:      row.StartedAt,
		EndedAt:        row.EndedAt,
		TotalComments:  row.TotalComments,
		TotalOrders:    row.TotalOrders,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}
