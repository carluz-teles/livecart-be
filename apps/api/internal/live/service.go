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

// =============================================================================
// LEGACY API - Creates an event + session + platform in one call
// =============================================================================

// Create creates a live event with an initial session and platform.
// This maintains backwards compatibility with the original /lives API.
func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	// 1. Create the event
	event, err := s.repo.CreateEvent(ctx, CreateEventParams{
		StoreID: input.StoreID,
		Title:   input.Title,
		Status:  "active",
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	// 2. Create the initial session
	session, err := s.repo.CreateSession(ctx, CreateSessionParams{
		EventID: event.ID,
		Status:  "active",
	})
	if err != nil {
		s.logger.Error("failed to create session after event",
			zap.String("event_id", event.ID),
			zap.Error(err),
		)
		return CreateLiveOutput{}, err
	}

	// 3. Add the platform to the session
	_, err = s.repo.AddPlatformToSession(ctx, session.ID, input.Platform, input.PlatformLiveID)
	if err != nil {
		s.logger.Error("failed to add platform after session",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
		return CreateLiveOutput{}, err
	}

	s.logger.Info("live created",
		zap.String("event_id", event.ID),
		zap.String("session_id", session.ID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return CreateLiveOutput{
		ID:        event.ID,
		Title:     event.Title,
		Platform:  input.Platform,
		Status:    event.Status,
		CreatedAt: event.CreatedAt,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id, storeID string) (LiveOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get platform info from first session
	var platform, platformLiveID string
	var startedAt, endedAt = event.CreatedAt, event.UpdatedAt
	var totalComments int

	if len(sessions) > 0 {
		firstSession := sessions[0]
		if firstSession.StartedAt != nil {
			startedAt = *firstSession.StartedAt
		}
		if firstSession.EndedAt != nil {
			endedAt = *firstSession.EndedAt
		}
		totalComments = firstSession.TotalComments

		platforms, err := s.repo.ListPlatformsBySession(ctx, firstSession.ID)
		if err == nil && len(platforms) > 0 {
			platform = platforms[0].Platform
			platformLiveID = platforms[0].PlatformLiveID
		}
	}

	return LiveOutput{
		ID:             event.ID,
		StoreID:        event.StoreID,
		Title:          event.Title,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
		Status:         event.Status,
		StartedAt:      &startedAt,
		EndedAt:        &endedAt,
		TotalComments:  totalComments,
		TotalOrders:    event.TotalOrders,
		CreatedAt:      event.CreatedAt,
		UpdatedAt:      event.UpdatedAt,
	}, nil
}

func (s *Service) List(ctx context.Context, input ListLivesInput) (ListLivesOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	lives, total, err := s.repo.ListLives(ctx, ListLivesParams{
		StoreID: input.StoreID,
		Search:  input.Search,
		Pagination: struct {
			Limit  int
			Offset int
		}{
			Limit:  input.Pagination.Limit,
			Offset: input.Pagination.Offset(),
		},
		Sorting: struct {
			SortBy    string
			SortOrder string
		}{
			SortBy:    input.Sorting.SortBy,
			SortOrder: input.Sorting.SortOrder,
		},
		Filters: input.Filters,
	})
	if err != nil {
		return ListLivesOutput{}, err
	}

	return ListLivesOutput{
		Lives:      lives,
		Total:      total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) Update(ctx context.Context, input UpdateLiveInput) (LiveOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Update event title
	event, err := s.repo.UpdateEventTitle(ctx, input.ID, input.Title)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get full live output
	return s.GetByID(ctx, event.ID, input.StoreID)
}

func (s *Service) Start(ctx context.Context, id, storeID string) (LiveOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return LiveOutput{}, err
	}

	// Get active session for this event
	session, err := s.repo.GetActiveSessionByEvent(ctx, id)
	if err != nil {
		return LiveOutput{}, err
	}
	if session == nil {
		return LiveOutput{}, fmt.Errorf("no active session found for event")
	}

	// Start the session
	_, err = s.repo.StartSession(ctx, session.ID)
	if err != nil {
		return LiveOutput{}, err
	}

	return s.GetByID(ctx, id, storeID)
}

func (s *Service) End(ctx context.Context, input EndLiveInput) (EndLiveOutput, error) {
	// 1. End the event
	event, err := s.repo.EndEvent(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}

	// 2. End all active sessions for this event
	sessions, err := s.repo.ListSessionsByEvent(ctx, input.ID)
	if err != nil {
		s.logger.Error("failed to list sessions for event",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	} else {
		for _, session := range sessions {
			if session.Status == "active" || session.Status == "live" {
				_, err := s.repo.EndSession(ctx, session.ID)
				if err != nil {
					s.logger.Error("failed to end session",
						zap.String("session_id", session.ID),
						zap.Error(err),
					)
				}
			}
		}
	}

	// 3. Finalize all pending carts (now tied to event)
	cartsFinalized, err := s.repo.FinalizeCartsByEvent(ctx, input.ID)
	if err != nil {
		s.logger.Error("failed to finalize carts",
			zap.String("event_id", input.ID),
			zap.Error(err),
		)
	}

	// 4. Determine if we should auto-send checkout links
	// Rule: auto-send only if event has 1 session (or crash recovery)
	sessionCount, _ := s.repo.CountSessionsByEvent(ctx, input.ID)
	autoSend := false

	if input.AutoSend != nil {
		// Use override value
		autoSend = *input.AutoSend
	} else if sessionCount <= 1 {
		// Single session event - use store default
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
	// Multi-session events: auto-send is disabled by default

	s.logger.Info("live event ended",
		zap.String("event_id", input.ID),
		zap.Int("session_count", sessionCount),
		zap.Int("carts_finalized", cartsFinalized),
		zap.Bool("auto_send_links", autoSend),
	)

	// Get full output
	liveOutput, _ := s.GetByID(ctx, event.ID, input.StoreID)

	return EndLiveOutput{
		Live:           liveOutput,
		CartsFinalized: cartsFinalized,
		AutoSendLinks:  autoSend,
	}, nil
}

func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.DeleteEvent(ctx, id, storeID)
}

func (s *Service) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	return s.repo.GetStats(ctx, storeID)
}

// =============================================================================
// SESSION OPERATIONS
// =============================================================================

// CreateSession creates a new session within an event.
func (s *Service) CreateSession(ctx context.Context, input CreateSessionInput) (CreateSessionOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return CreateSessionOutput{}, err
	}

	// Create the session
	session, err := s.repo.CreateSession(ctx, CreateSessionParams{
		EventID: input.EventID,
		Status:  "active",
	})
	if err != nil {
		return CreateSessionOutput{}, err
	}

	// Add the platform
	platform, err := s.repo.AddPlatformToSession(ctx, session.ID, input.Platform, input.PlatformLiveID)
	if err != nil {
		s.logger.Error("failed to add platform to session",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
		return CreateSessionOutput{}, err
	}

	s.logger.Info("session created",
		zap.String("event_id", input.EventID),
		zap.String("session_id", session.ID),
		zap.String("platform", input.Platform),
	)

	return CreateSessionOutput{
		ID:      session.ID,
		EventID: session.EventID,
		Status:  session.Status,
		Platform: PlatformOutput{
			ID:             platform.ID,
			SessionID:      platform.SessionID,
			Platform:       platform.Platform,
			PlatformLiveID: platform.PlatformLiveID,
			AddedAt:        platform.AddedAt,
		},
		CreatedAt: session.CreatedAt,
	}, nil
}

// GetSessionByPlatformLiveID finds an active session by platform live ID.
func (s *Service) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionOutput, error) {
	session, err := s.repo.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	// Get platforms for this session
	platforms, err := s.repo.ListPlatformsBySession(ctx, session.ID)
	if err != nil {
		return nil, err
	}

	platformOutputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		platformOutputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return &SessionOutput{
		ID:            session.ID,
		EventID:       session.EventID,
		Status:        session.Status,
		StartedAt:     session.StartedAt,
		EndedAt:       session.EndedAt,
		TotalComments: session.TotalComments,
		Platforms:     platformOutputs,
		CreatedAt:     session.CreatedAt,
		UpdatedAt:     session.UpdatedAt,
	}, nil
}

// GetEventByPlatformLiveID finds an active event by any associated platform live ID.
func (s *Service) GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventOutput, error) {
	event, err := s.repo.GetEventByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, nil
	}

	return &EventOutput{
		ID:          event.ID,
		StoreID:     event.StoreID,
		Title:       event.Title,
		Status:      event.Status,
		TotalOrders: event.TotalOrders,
		CreatedAt:   event.CreatedAt,
		UpdatedAt:   event.UpdatedAt,
	}, nil
}

// =============================================================================
// PLATFORM OPERATIONS
// =============================================================================

// AddPlatform adds a platform ID to a session.
func (s *Service) AddPlatform(ctx context.Context, input AddPlatformInput) (PlatformOutput, error) {
	row, err := s.repo.AddPlatformToSession(ctx, input.SessionID, input.Platform, input.PlatformLiveID)
	if err != nil {
		return PlatformOutput{}, err
	}

	s.logger.Info("platform added to session",
		zap.String("session_id", input.SessionID),
		zap.String("platform", input.Platform),
		zap.String("platform_live_id", input.PlatformLiveID),
	)

	return PlatformOutput{
		ID:             row.ID,
		SessionID:      row.SessionID,
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt,
	}, nil
}

// ListPlatforms returns all platforms for a session.
func (s *Service) ListPlatforms(ctx context.Context, sessionID string) ([]PlatformOutput, error) {
	platforms, err := s.repo.ListPlatformsBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	outputs := make([]PlatformOutput, len(platforms))
	for i, p := range platforms {
		outputs[i] = PlatformOutput{
			ID:             p.ID,
			SessionID:      p.SessionID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return outputs, nil
}

// RemovePlatform removes a platform from a session.
func (s *Service) RemovePlatform(ctx context.Context, sessionID, platformLiveID string) error {
	return s.repo.RemovePlatformFromSession(ctx, sessionID, platformLiveID)
}

// =============================================================================
// CART OPERATIONS
// =============================================================================

// AddToCart adds a product to a user's cart during a live event.
// Creates a new cart if one doesn't exist for this user in this event.
func (s *Service) AddToCart(ctx context.Context, input AddToCartInput) (AddToCartOutput, error) {
	// Generate token for new carts
	token, err := generateCartToken()
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("generating cart token: %w", err)
	}

	// Get or create cart for this user in this event
	cart, isNew, err := s.repo.GetOrCreateCart(ctx, GetOrCreateCartParams{
		EventID:        input.EventID,
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
		zap.String("event_id", input.EventID),
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
