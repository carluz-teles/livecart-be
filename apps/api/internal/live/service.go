package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// Notifier is the minimal notification surface this package depends on.
// The concrete implementation lives in the integration package; we declare
// a local interface to avoid an import cycle.
type Notifier interface {
	NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) error
}

// NotifyEventCheckoutParams mirrors the integration package params struct
// (Go duck typing only matches methods, so we declare the input shape here).
type NotifyEventCheckoutParams struct {
	StoreID        string
	EventID        string
	CartID         string
	PlatformUserID string
	PlatformHandle string
	TotalItems     int
	TotalValue     int64
}

// ERPFinalizer is called at event end to reverse stock reservations and
// create final sales orders in the ERP. Local interface to avoid import cycle.
type ERPFinalizer interface {
	FinalizeEventERP(ctx context.Context, storeID, eventID string) error
}

type Service struct {
	repo          *Repository
	logger        *zap.Logger
	notifier      Notifier
	erpFinalizer  ERPFinalizer
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("live"),
	}
}

// SetNotifier wires a Notifier into the service after construction. This
// breaks the dependency cycle between live and integration packages
// (integration.Service depends on live.Service, and the notifier impl
// depends on integration.Service).
func (s *Service) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetERPFinalizer wires an ERPFinalizer into the service after construction.
func (s *Service) SetERPFinalizer(f ERPFinalizer) {
	s.erpFinalizer = f
}

// =============================================================================
// LEGACY API - Creates an event + session + platform in one call
// =============================================================================

// Create creates a live event with an optional initial session and platform.
// This maintains backwards compatibility with the original /lives API.
func (s *Service) Create(ctx context.Context, input CreateLiveInput) (CreateLiveOutput, error) {
	// Default to single type if not specified
	eventType := input.Type
	if eventType == "" {
		eventType = "single"
	}

	// Default close_cart_on_event_end to true if not specified
	closeCartOnEventEnd := true
	if input.CloseCartOnEventEnd != nil {
		closeCartOnEventEnd = *input.CloseCartOnEventEnd
	}

	// Determine initial status based on scheduling
	status := "active"
	if input.ScheduledAt != nil {
		status = "scheduled"
	}

	// 1. Create the event
	event, err := s.repo.CreateEvent(ctx, CreateEventParams{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		Type:                   eventType,
		Status:                 status,
		CloseCartOnEventEnd:    closeCartOnEventEnd,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
		SendOnLiveEnd:          input.SendOnLiveEnd,
		ScheduledAt:            input.ScheduledAt,
		Description:            input.Description,
	})
	if err != nil {
		return CreateLiveOutput{}, err
	}

	// If platform info is provided, create session and add platform
	var platform string
	if input.Platform != nil && input.PlatformLiveID != nil && *input.Platform != "" && *input.PlatformLiveID != "" {
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
		_, err = s.repo.AddPlatformToSession(ctx, session.ID, *input.Platform, *input.PlatformLiveID)
		if err != nil {
			s.logger.Error("failed to add platform after session",
				zap.String("session_id", session.ID),
				zap.Error(err),
			)
			return CreateLiveOutput{}, err
		}

		platform = *input.Platform

		s.logger.Info("live created with session",
			zap.String("event_id", event.ID),
			zap.String("session_id", session.ID),
			zap.String("platform", *input.Platform),
			zap.String("platform_live_id", *input.PlatformLiveID),
		)
	} else {
		s.logger.Info("live created without session",
			zap.String("event_id", event.ID),
			zap.String("type", eventType),
		)
	}

	return CreateLiveOutput{
		ID:        event.ID,
		Title:     event.Title,
		Type:      event.Type,
		Platform:  platform,
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
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Platform:               platform,
		PlatformLiveID:         platformLiveID,
		Status:                 event.Status,
		StartedAt:              &startedAt,
		EndedAt:                &endedAt,
		TotalComments:          totalComments,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:  event.SendOnLiveEnd,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
	}, nil
}

// GetEventWithSessions returns an event with all its sessions and platforms
func (s *Service) GetEventWithSessions(ctx context.Context, id, storeID string) (EventOutput, error) {
	event, err := s.repo.GetEventByID(ctx, id, storeID)
	if err != nil {
		return EventOutput{}, err
	}

	// Get sessions for this event
	sessionRows, err := s.repo.ListSessionsByEvent(ctx, event.ID)
	if err != nil {
		return EventOutput{}, err
	}

	// Build session outputs with platforms and stats
	sessions := make([]SessionOutput, len(sessionRows))
	for i, sessionRow := range sessionRows {
		platforms, err := s.repo.ListPlatformsBySession(ctx, sessionRow.ID)
		if err != nil {
			s.logger.Warn("failed to list platforms for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
			platforms = []PlatformRow{}
		}

		platformOutputs := make([]PlatformOutput, len(platforms))
		for j, p := range platforms {
			platformOutputs[j] = PlatformOutput{
				ID:             p.ID,
				SessionID:      p.SessionID,
				Platform:       p.Platform,
				PlatformLiveID: p.PlatformLiveID,
				AddedAt:        p.AddedAt,
			}
		}

		// Get session stats (carts and revenue)
		var totalCarts, paidCarts int
		var totalRevenue, paidRevenue int64
		stats, err := s.repo.GetSessionStats(ctx, sessionRow.ID)
		if err != nil {
			s.logger.Warn("failed to get session stats",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
		} else {
			totalCarts = stats.TotalCarts
			paidCarts = stats.PaidCarts
			totalRevenue = stats.TotalRevenue
			paidRevenue = stats.PaidRevenue
		}

		// Get comments for this session (limit 100 for performance)
		var commentOutputs []CommentOutput
		commentRows, err := s.repo.ListCommentsBySession(ctx, sessionRow.ID, 100, 0)
		if err != nil {
			s.logger.Warn("failed to list comments for session",
				zap.String("session_id", sessionRow.ID),
				zap.Error(err),
			)
		} else {
			commentOutputs = make([]CommentOutput, len(commentRows))
			for k, c := range commentRows {
				commentOutputs[k] = CommentOutput{
					Handle: c.PlatformHandle,
					Text:   c.Text,
				}
			}
		}

		sessions[i] = SessionOutput{
			ID:            sessionRow.ID,
			EventID:       sessionRow.EventID,
			Status:        sessionRow.Status,
			StartedAt:     sessionRow.StartedAt,
			EndedAt:       sessionRow.EndedAt,
			TotalComments: sessionRow.TotalComments,
			TotalCarts:    totalCarts,
			PaidCarts:     paidCarts,
			TotalRevenue:  totalRevenue,
			PaidRevenue:   paidRevenue,
			Platforms:     platformOutputs,
			Comments:      commentOutputs,
			CreatedAt:     sessionRow.CreatedAt,
			UpdatedAt:     sessionRow.UpdatedAt,
		}
	}

	// Get product and upsell counts
	productCount, err := s.repo.CountEventProducts(ctx, event.ID)
	if err != nil {
		s.logger.Warn("failed to count event products", zap.String("event_id", event.ID), zap.Error(err))
	}
	upsellCount, err := s.repo.CountEventUpsells(ctx, event.ID)
	if err != nil {
		s.logger.Warn("failed to count event upsells", zap.String("event_id", event.ID), zap.Error(err))
	}

	return EventOutput{
		ID:                     event.ID,
		StoreID:                event.StoreID,
		Title:                  event.Title,
		Type:                   event.Type,
		Status:                 event.Status,
		TotalOrders:            event.TotalOrders,
		CloseCartOnEventEnd:    event.CloseCartOnEventEnd,
		CartExpirationMinutes:  event.CartExpirationMinutes,
		CartMaxQuantityPerItem: event.CartMaxQuantityPerItem,
		SendOnLiveEnd:          event.SendOnLiveEnd,
		ScheduledAt:            event.ScheduledAt,
		Description:            event.Description,
		ProductCount:           productCount,
		UpsellCount:            upsellCount,
		Sessions:               sessions,
		CreatedAt:              event.CreatedAt,
		UpdatedAt:              event.UpdatedAt,
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
	// 0. Idempotency guard: if the event was already ended, do not re-run
	// the finalization side-effects (carts, DMs). Return the current state.
	existing, err := s.repo.GetEventByID(ctx, input.ID, input.StoreID)
	if err != nil {
		return EndLiveOutput{}, err
	}
	if existing != nil && existing.Status == "ended" {
		liveOutput, _ := s.GetByID(ctx, existing.ID, input.StoreID)
		return EndLiveOutput{
			Live:           liveOutput,
			CartsFinalized: 0,
			AutoSendLinks:  false,
		}, nil
	}

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

	// 3.5. Reverse ERP stock reservations and create final sales orders (async — never blocks the response).
	if s.erpFinalizer != nil {
		go func() {
			bgCtx := context.Background()
			if err := s.erpFinalizer.FinalizeEventERP(bgCtx, input.StoreID, event.ID); err != nil {
				s.logger.Error("failed to finalize ERP for event",
					zap.String("event_id", event.ID),
					zap.Error(err),
				)
			}
		}()
	}

	// 4. Determine if we should auto-send checkout links.
	// Carts are unique per (event_id, platform_user_id), so multi-session
	// events already aggregate items per buyer. Use the explicit override
	// when provided, otherwise fall back to the store default.
	sessionCount, _ := s.repo.CountSessionsByEvent(ctx, input.ID)
	autoSend := false
	if input.AutoSend != nil {
		autoSend = *input.AutoSend
	} else {
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

	s.logger.Info("live event ended",
		zap.String("event_id", input.ID),
		zap.Int("session_count", sessionCount),
		zap.Int("carts_finalized", cartsFinalized),
		zap.Bool("auto_send_links", autoSend),
	)

	// 5. Dispatch checkout DMs (best-effort, async — never blocks the response).
	if autoSend && s.notifier != nil {
		go s.sendCheckoutLinksForEvent(context.Background(), input.StoreID, event.ID)
	}

	// Get full output
	liveOutput, _ := s.GetByID(ctx, event.ID, input.StoreID)

	return EndLiveOutput{
		Live:           liveOutput,
		CartsFinalized: cartsFinalized,
		AutoSendLinks:  autoSend,
	}, nil
}

// sendCheckoutLinksForEvent iterates over all carts of an event with at least
// one item and dispatches a checkout DM per buyer through the configured
// Notifier. Errors are logged individually and never interrupt the loop.
func (s *Service) sendCheckoutLinksForEvent(ctx context.Context, storeID, eventID string) {
	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		s.logger.Error("failed to list carts for event checkout dispatch",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return
	}

	sent := 0
	skipped := 0
	for _, c := range carts {
		if c.TotalItems <= 0 || c.PlatformUserID == "" {
			skipped++
			continue
		}
		// Only notify carts that were just finalized for checkout. Skip
		// carts already paid, expired, or abandoned during the live.
		if c.Status != "checkout" {
			skipped++
			continue
		}
		if c.PaymentStatus != nil && *c.PaymentStatus == "paid" {
			skipped++
			continue
		}
		if err := s.notifier.NotifyEventCheckout(ctx, NotifyEventCheckoutParams{
			StoreID:        storeID,
			EventID:        eventID,
			CartID:         c.ID,
			PlatformUserID: c.PlatformUserID,
			PlatformHandle: c.PlatformHandle,
			TotalItems:     c.TotalItems,
			TotalValue:     c.TotalValue,
		}); err != nil {
			s.logger.Warn("failed to notify event checkout",
				zap.String("event_id", eventID),
				zap.String("cart_id", c.ID),
				zap.String("platform_user_id", c.PlatformUserID),
				zap.Error(err),
			)
			continue
		}
		sent++
	}

	s.logger.Info("event checkout dispatch finished",
		zap.String("event_id", eventID),
		zap.Int("sent", sent),
		zap.Int("skipped", skipped),
		zap.Int("total", len(carts)),
	)
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
		ID:                      event.ID,
		StoreID:                 event.StoreID,
		Title:                   event.Title,
		Type:                    event.Type,
		Status:                  event.Status,
		TotalOrders:             event.TotalOrders,
		CloseCartOnEventEnd:     event.CloseCartOnEventEnd,
		CartExpirationMinutes:   event.CartExpirationMinutes,
		CartMaxQuantityPerItem:  event.CartMaxQuantityPerItem,
		SendOnLiveEnd:           event.SendOnLiveEnd,
		CurrentActiveProductID:  event.CurrentActiveProductID,
		ProcessingPaused:        event.ProcessingPaused,
		ScheduledAt:             event.ScheduledAt,
		Description:             event.Description,
		CreatedAt:               event.CreatedAt,
		UpdatedAt:               event.UpdatedAt,
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
		CartID:     cart.ID,
		ProductID:  input.ProductID,
		Quantity:   input.Quantity,
		UnitPrice:  input.ProductPrice,
		Waitlisted: input.Waitlisted,
	})
	if err != nil {
		return AddToCartOutput{}, fmt.Errorf("adding item to cart: %w", err)
	}

	// Get updated cart totals
	totalItems, totalCents, err := s.repo.GetCartTotals(ctx, cart.ID)
	if err != nil {
		s.logger.Warn("failed to get cart totals", zap.Error(err))
		// Continue with zero totals - notification can still be sent
	}

	s.logger.Info("added product to cart",
		zap.String("cart_id", cart.ID),
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
		zap.Int("quantity", input.Quantity),
		zap.Bool("new_cart", isNew),
		zap.Int("total_items", totalItems),
		zap.Int64("total_cents", totalCents),
	)

	return AddToCartOutput{
		CartID:     cart.ID,
		CartToken:  cart.Token,
		IsNewCart:  isNew,
		TotalItems: totalItems,
		TotalCents: totalCents,
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
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

// GetEventStats returns stats for an event (comments, carts, revenue).
func (s *Service) GetEventStats(ctx context.Context, eventID, storeID string) (EventStatsOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	stats, err := s.repo.GetEventStats(ctx, eventID)
	if err != nil {
		return EventStatsOutput{}, err
	}

	return EventStatsOutput{
		TotalComments:     stats.TotalComments,
		TotalCarts:        stats.TotalCarts,
		OpenCarts:         stats.OpenCarts,
		CheckoutCarts:     stats.CheckoutCarts,
		PaidCarts:         stats.PaidCarts,
		TotalProductsSold: stats.TotalProductsSold,
		ProjectedRevenue:  stats.ProjectedRevenue,
		ConfirmedRevenue:  stats.ConfirmedRevenue,
	}, nil
}

// ListCartsWithTotalByEvent returns all carts for an event with total value and item count.
func (s *Service) ListCartsWithTotalByEvent(ctx context.Context, eventID, storeID string) ([]CartWithTotalOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	carts, err := s.repo.ListCartsWithTotalByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]CartWithTotalOutput, len(carts))
	for i, cart := range carts {
		outputs[i] = CartWithTotalOutput{
			ID:             cart.ID,
			SessionID:      cart.SessionID,
			PlatformUserID: cart.PlatformUserID,
			PlatformHandle: cart.PlatformHandle,
			Status:         cart.Status,
			PaymentStatus:  cart.PaymentStatus,
			TotalValue:     cart.TotalValue,
			TotalItems:     cart.TotalItems,
			CreatedAt:      cart.CreatedAt,
			ExpiresAt:      cart.ExpiresAt,
		}
	}

	return outputs, nil
}

// ListProductsByEvent returns all products sold in an event with quantity and revenue.
func (s *Service) ListProductsByEvent(ctx context.Context, eventID, storeID string) ([]EventProductSalesOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	products, err := s.repo.ListProductsByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	outputs := make([]EventProductSalesOutput, len(products))
	for i, product := range products {
		outputs[i] = EventProductSalesOutput{
			ID:            product.ID,
			Name:          product.Name,
			ImageURL:      product.ImageURL,
			Keyword:       product.Keyword,
			TotalQuantity: product.TotalQuantity,
			TotalRevenue:  product.TotalRevenue,
		}
	}

	return outputs, nil
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// SetActiveProduct sets or clears the active product for an event
func (s *Service) SetActiveProduct(ctx context.Context, eventID, storeID string, productID *string) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.ErrBadRequest("can only set active product on active events")
	}

	// Set or clear active product
	if productID != nil && *productID != "" {
		_, err = s.repo.SetActiveProduct(ctx, eventID, storeID, *productID)
	} else {
		_, err = s.repo.ClearActiveProduct(ctx, eventID, storeID)
	}
	if err != nil {
		return nil, err
	}

	s.logger.Info("active product updated",
		zap.String("event_id", eventID),
		zap.Stringp("product_id", productID),
	)

	// Return updated state
	return s.GetLiveModeState(ctx, eventID, storeID)
}

// SetProcessingPaused pauses or resumes comment processing for an event
func (s *Service) SetProcessingPaused(ctx context.Context, eventID, storeID string, paused bool) (*LiveModeStateOutput, error) {
	// Verify event exists and is active
	event, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	if event.Status != "active" {
		return nil, httpx.ErrBadRequest("can only change processing state on active events")
	}

	_, err = s.repo.SetProcessingPaused(ctx, eventID, storeID, paused)
	if err != nil {
		return nil, err
	}

	s.logger.Info("processing paused state updated",
		zap.String("event_id", eventID),
		zap.Bool("paused", paused),
	)

	// Return updated state
	return s.GetLiveModeState(ctx, eventID, storeID)
}

// GetLiveModeState returns the current live mode state for an event
func (s *Service) GetLiveModeState(ctx context.Context, eventID, storeID string) (*LiveModeStateOutput, error) {
	return s.repo.GetLiveModeState(ctx, eventID, storeID)
}

// =============================================================================
// EVENT PRODUCTS (Whitelist)
// =============================================================================

// AddEventProduct adds a product to an event's whitelist
func (s *Service) AddEventProduct(ctx context.Context, input AddEventProductInput) (EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventProductOutput{}, err
	}

	output, err := s.repo.AddEventProduct(ctx, input)
	if err != nil {
		return EventProductOutput{}, err
	}

	s.logger.Info("added product to event whitelist",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
	)

	return output, nil
}

// ListEventProducts returns all products in an event's whitelist
func (s *Service) ListEventProducts(ctx context.Context, eventID, storeID string) ([]EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	return s.repo.ListEventProducts(ctx, eventID)
}

// UpdateEventProduct updates a product's configuration in an event
func (s *Service) UpdateEventProduct(ctx context.Context, input UpdateEventProductInput) (EventProductOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventProductOutput{}, err
	}

	output, err := s.repo.UpdateEventProduct(ctx, input)
	if err != nil {
		return EventProductOutput{}, err
	}

	s.logger.Info("updated event product",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ID),
	)

	return output, nil
}

// DeleteEventProduct removes a product from an event's whitelist
func (s *Service) DeleteEventProduct(ctx context.Context, id, eventID, storeID string) error {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return err
	}

	if err := s.repo.DeleteEventProduct(ctx, id, eventID); err != nil {
		return err
	}

	s.logger.Info("deleted event product",
		zap.String("event_id", eventID),
		zap.String("product_id", id),
	)

	return nil
}

// ValidateProductForEvent checks if a product can be sold in an event
func (s *Service) ValidateProductForEvent(ctx context.Context, eventID, productID, storeID string) (*ProductValidationResult, error) {
	return s.repo.GetEventProductConfig(ctx, eventID, productID, storeID)
}

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// AddEventUpsell adds an upsell to an event
func (s *Service) AddEventUpsell(ctx context.Context, input AddEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.AddEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	s.logger.Info("added upsell to event",
		zap.String("event_id", input.EventID),
		zap.String("product_id", input.ProductID),
	)

	return output, nil
}

// ListEventUpsells returns all upsells for an event
func (s *Service) ListEventUpsells(ctx context.Context, eventID, storeID string) ([]EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return nil, err
	}

	return s.repo.ListEventUpsells(ctx, eventID)
}

// UpdateEventUpsell updates an upsell's configuration
func (s *Service) UpdateEventUpsell(ctx context.Context, input UpdateEventUpsellInput) (EventUpsellOutput, error) {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, input.EventID, input.StoreID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	output, err := s.repo.UpdateEventUpsell(ctx, input)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	s.logger.Info("updated event upsell",
		zap.String("event_id", input.EventID),
		zap.String("upsell_id", input.ID),
	)

	return output, nil
}

// DeleteEventUpsell removes an upsell from an event
func (s *Service) DeleteEventUpsell(ctx context.Context, id, eventID, storeID string) error {
	// Verify event exists and belongs to store
	_, err := s.repo.GetEventByID(ctx, eventID, storeID)
	if err != nil {
		return err
	}

	if err := s.repo.DeleteEventUpsell(ctx, id, eventID); err != nil {
		return err
	}

	s.logger.Info("deleted event upsell",
		zap.String("event_id", eventID),
		zap.String("upsell_id", id),
	)

	return nil
}
