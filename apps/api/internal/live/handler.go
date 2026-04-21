package live

import (
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/lives")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
	g.Post("/:id/start", h.Start)
	g.Post("/:id/end", h.End)

	// Event details endpoints
	g.Get("/:id/event-stats", h.GetEventStats)
	g.Get("/:id/carts", h.ListCarts)
	g.Get("/:id/products", h.ListProducts)

	// Session management within an event
	g.Post("/:id/sessions", h.CreateSession)

	// Platform aggregation (on sessions)
	g.Get("/:id/platforms", h.ListPlatforms)
	g.Post("/:id/platforms", h.AddPlatform)
	g.Delete("/:id/platforms/:platformLiveId", h.RemovePlatform)

	// Live Mode - Active Product and Processing Control
	g.Get("/:id/live-mode", h.GetLiveModeState)
	g.Patch("/:id/active-product", h.SetActiveProduct)
	g.Patch("/:id/pause-processing", h.SetProcessingPaused)

	// Event Products (Whitelist)
	g.Get("/:id/whitelist", h.ListEventProducts)
	g.Post("/:id/whitelist", h.AddEventProduct)
	g.Put("/:id/whitelist/:productId", h.UpdateEventProduct)
	g.Delete("/:id/whitelist/:productId", h.DeleteEventProduct)

	// Event Upsells
	g.Get("/:id/upsells", h.ListEventUpsells)
	g.Post("/:id/upsells", h.AddEventUpsell)
	g.Put("/:id/upsells/:upsellId", h.UpdateEventUpsell)
	g.Delete("/:id/upsells/:upsellId", h.DeleteEventUpsell)
}

// Create godoc
// @Summary      Create a new live event
// @Description  Creates a live event with an initial session and platform
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateLiveRequest true "Live creation payload"
// @Success      201 {object} httpx.Envelope{data=CreateLiveResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateLiveRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	storeID := c.Locals("store_id").(string)

	// Parse scheduling time if provided
	var scheduledAt *time.Time
	if req.ScheduledAt != nil && *req.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ScheduledAt)
		if err != nil {
			return httpx.BadRequest(c, "invalid scheduledAt format, use ISO8601/RFC3339")
		}
		scheduledAt = &t
	}

	output, err := h.service.Create(c.Context(), CreateLiveInput{
		StoreID:                storeID,
		Title:                  req.Title,
		Type:                   req.Type,
		Platform:               req.Platform,
		PlatformLiveID:         req.PlatformLiveID,
		CloseCartOnEventEnd:    req.CloseCartOnEventEnd,
		CartExpirationMinutes:  req.CartExpirationMinutes,
		CartMaxQuantityPerItem: req.CartMaxQuantityPerItem,
		SendOnLiveEnd:          req.SendOnLiveEnd,
		ScheduledAt:            scheduledAt,
		Description:            req.Description,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CreateLiveResponse{
		ID:        output.ID,
		Title:     output.Title,
		Type:      output.Type,
		Platform:  output.Platform,
		Status:    output.Status,
		CreatedAt: output.CreatedAt,
	})
}

// GetByID godoc
// @Summary      Get live event by ID
// @Description  Returns a single live event by its UUID with all sessions
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=EventResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetEventWithSessions(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toEventResponse(output))
}

// List godoc
// @Summary      List live events
// @Description  Returns live events with filtering, pagination, and sorting
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by title"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by status"
// @Success      200 {object} httpx.Envelope{data=ListLivesResponse}
// @Router       /api/v1/stores/{storeId}/lives [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	input := ListLivesInput{
		StoreID: storeID,
		Search:  c.Query("search"),
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
		Sorting: query.Sorting{
			SortBy:    c.Query("sortBy", "created_at"),
			SortOrder: c.Query("sortOrder", "desc"),
		},
		Filters: parseLiveFilters(c),
	}

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]LiveResponse, len(output.Lives))
	for i, o := range output.Lives {
		responses[i] = toLiveResponse(o)
	}

	return httpx.OK(c, ListLivesResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

// Delete godoc
// @Summary      Delete a live event
// @Description  Deletes a live event by its UUID
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id} [delete]
// @Security     BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	if err := h.service.Delete(c.Context(), id, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, id)
}

// Update godoc
// @Summary      Update a live event
// @Description  Updates an existing live event by its UUID
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Param        request body UpdateLiveRequest true "Live event update payload"
// @Success      200 {object} httpx.Envelope{data=LiveResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id} [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	var req UpdateLiveRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Update(c.Context(), UpdateLiveInput{
		StoreID: storeID,
		ID:      id,
		Title:   req.Title,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveResponse(output))
}

// Start godoc
// @Summary      Start a live session
// @Description  Starts the active session of a live event
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=LiveResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/start [post]
// @Security     BearerAuth
func (h *Handler) Start(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.Start(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveResponse(output))
}

// End godoc
// @Summary      End a live event
// @Description  Ends a live event and finalizes all pending carts
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Param        request body EndLiveRequest false "End live options"
// @Success      200 {object} httpx.Envelope{data=EndLiveResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/end [post]
// @Security     BearerAuth
func (h *Handler) End(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	// Parse optional request body
	var req EndLiveRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return httpx.BadRequest(c, "invalid request body")
		}
	}

	output, err := h.service.End(c.Context(), EndLiveInput{
		ID:       id,
		StoreID:  storeID,
		AutoSend: req.SendOnLiveEnd,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, EndLiveResponse{
		Live:           toLiveResponse(output.Live),
		CartsFinalized: output.CartsFinalized,
		AutoSendLinks:  output.AutoSendLinks,
	})
}

// GetStats godoc
// @Summary      Get live statistics
// @Description  Returns aggregated statistics for all live events in the store
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=LiveStatsResponse}
// @Router       /api/v1/stores/{storeId}/lives/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	output, err := h.service.GetStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, LiveStatsResponse{
		TotalLives:  output.TotalLives,
		ActiveLives: output.ActiveLives,
		TotalOrders: output.TotalOrders,
	})
}

// =============================================================================
// SESSION MANAGEMENT
// =============================================================================

// CreateSession godoc
// @Summary      Create a new session within an event
// @Description  Creates a new session for an existing event (for multi-session events)
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Param        request body CreateSessionRequest true "Session creation payload"
// @Success      201 {object} httpx.Envelope{data=SessionResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/sessions [post]
// @Security     BearerAuth
func (h *Handler) CreateSession(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req CreateSessionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.CreateSession(c.Context(), CreateSessionInput{
		EventID:        eventID,
		StoreID:        storeID,
		Platform:       req.Platform,
		PlatformLiveID: req.PlatformLiveID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, SessionResponse{
		ID:        output.ID,
		EventID:   output.EventID,
		Status:    output.Status,
		CreatedAt: output.CreatedAt,
		UpdatedAt: output.CreatedAt,
		Platforms: []PlatformResponse{{
			ID:             output.Platform.ID,
			Platform:       output.Platform.Platform,
			PlatformLiveID: output.Platform.PlatformLiveID,
			AddedAt:        output.Platform.AddedAt,
		}},
	})
}

// =============================================================================
// PLATFORM AGGREGATION
// =============================================================================

// ListPlatforms godoc
// @Summary      List platforms for the active session of an event
// @Description  Returns all platform IDs associated with the active session
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=ListPlatformsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms [get]
// @Security     BearerAuth
func (h *Handler) ListPlatforms(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	// Get live to find active session
	live, err := h.service.GetByID(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Get session by event
	session, err := h.service.repo.GetActiveSessionByEvent(c.Context(), live.ID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if session == nil {
		return httpx.OK(c, ListPlatformsResponse{Data: []PlatformResponse{}})
	}

	platforms, err := h.service.ListPlatforms(c.Context(), session.ID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]PlatformResponse, len(platforms))
	for i, p := range platforms {
		responses[i] = PlatformResponse{
			ID:             p.ID,
			Platform:       p.Platform,
			PlatformLiveID: p.PlatformLiveID,
			AddedAt:        p.AddedAt,
		}
	}

	return httpx.OK(c, ListPlatformsResponse{Data: responses})
}

// AddPlatform godoc
// @Summary      Add a platform to the active session of an event
// @Description  Associates a new platform live ID with the active session (for crash recovery)
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Param        request body AddPlatformRequest true "Platform to add"
// @Success      201 {object} httpx.Envelope{data=PlatformResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms [post]
// @Security     BearerAuth
func (h *Handler) AddPlatform(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req AddPlatformRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Get live to find active session
	live, err := h.service.GetByID(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Get active session
	session, err := h.service.repo.GetActiveSessionByEvent(c.Context(), live.ID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if session == nil {
		return httpx.BadRequest(c, "no active session found for this event")
	}

	output, err := h.service.AddPlatform(c.Context(), AddPlatformInput{
		SessionID:      session.ID,
		Platform:       req.Platform,
		PlatformLiveID: req.PlatformLiveID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, PlatformResponse{
		ID:             output.ID,
		Platform:       output.Platform,
		PlatformLiveID: output.PlatformLiveID,
		AddedAt:        output.AddedAt,
	})
}

// RemovePlatform godoc
// @Summary      Remove a platform from the active session
// @Description  Disassociates a platform live ID from the active session
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Param        platformLiveId path string true "Platform live ID to remove"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms/{platformLiveId} [delete]
// @Security     BearerAuth
func (h *Handler) RemovePlatform(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")
	platformLiveID := c.Params("platformLiveId")

	// Get live to find active session
	live, err := h.service.GetByID(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Get active session
	session, err := h.service.repo.GetActiveSessionByEvent(c.Context(), live.ID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	if session == nil {
		return httpx.BadRequest(c, "no active session found for this event")
	}

	if err := h.service.RemovePlatform(c.Context(), session.ID, platformLiveID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, platformLiveID)
}

func parseLiveFilters(c *fiber.Ctx) LiveFilters {
	var filters LiveFilters

	statusBytes := c.Context().QueryArgs().PeekMulti("status")
	if len(statusBytes) > 0 {
		filters.Status = make([]string, len(statusBytes))
		for i, s := range statusBytes {
			filters.Status[i] = string(s)
		}
	}

	platformBytes := c.Context().QueryArgs().PeekMulti("platform")
	if len(platformBytes) > 0 {
		filters.Platform = make([]string, len(platformBytes))
		for i, p := range platformBytes {
			filters.Platform[i] = string(p)
		}
	}

	if dateFrom := c.Query("dateFrom"); dateFrom != "" {
		filters.DateFrom = &dateFrom
	}
	if dateTo := c.Query("dateTo"); dateTo != "" {
		filters.DateTo = &dateTo
	}

	return filters
}

func toLiveResponse(o LiveOutput) LiveResponse {
	return LiveResponse{
		ID:                     o.ID,
		Title:                  o.Title,
		Type:                   o.Type,
		Platform:               o.Platform,
		PlatformLiveID:         o.PlatformLiveID,
		Status:                 o.Status,
		StartedAt:              o.StartedAt,
		EndedAt:                o.EndedAt,
		TotalComments:          o.TotalComments,
		TotalOrders:            o.TotalOrders,
		CloseCartOnEventEnd:    o.CloseCartOnEventEnd,
		CartExpirationMinutes:  o.CartExpirationMinutes,
		CartMaxQuantityPerItem: o.CartMaxQuantityPerItem,
		SendOnLiveEnd:          o.SendOnLiveEnd,
		ScheduledAt:            o.ScheduledAt,
		Description:            o.Description,
		ProductCount:           o.ProductCount,
		UpsellCount:            o.UpsellCount,
		CreatedAt:              o.CreatedAt,
		UpdatedAt:              o.UpdatedAt,
	}
}

func toEventResponse(o EventOutput) EventResponse {
	sessions := make([]SessionResponse, len(o.Sessions))
	for i, s := range o.Sessions {
		platforms := make([]PlatformResponse, len(s.Platforms))
		for j, p := range s.Platforms {
			platforms[j] = PlatformResponse{
				ID:             p.ID,
				Platform:       p.Platform,
				PlatformLiveID: p.PlatformLiveID,
				AddedAt:        p.AddedAt,
			}
		}

		comments := make([]CommentResponse, len(s.Comments))
		for k, c := range s.Comments {
			comments[k] = CommentResponse{
				Handle: c.Handle,
				Text:   c.Text,
			}
		}

		sessions[i] = SessionResponse{
			ID:            s.ID,
			EventID:       s.EventID,
			Status:        s.Status,
			StartedAt:     s.StartedAt,
			EndedAt:       s.EndedAt,
			TotalComments: s.TotalComments,
			TotalCarts:    s.TotalCarts,
			PaidCarts:     s.PaidCarts,
			TotalRevenue:  s.TotalRevenue,
			PaidRevenue:   s.PaidRevenue,
			Platforms:     platforms,
			Comments:      comments,
			CreatedAt:     s.CreatedAt,
			UpdatedAt:     s.UpdatedAt,
		}
	}

	return EventResponse{
		ID:                     o.ID,
		Title:                  o.Title,
		Type:                   o.Type,
		Status:                 o.Status,
		TotalOrders:            o.TotalOrders,
		CloseCartOnEventEnd:    o.CloseCartOnEventEnd,
		CartExpirationMinutes:  o.CartExpirationMinutes,
		CartMaxQuantityPerItem: o.CartMaxQuantityPerItem,
		SendOnLiveEnd:          o.SendOnLiveEnd,
		ScheduledAt:            o.ScheduledAt,
		Description:            o.Description,
		ProductCount:           o.ProductCount,
		UpsellCount:            o.UpsellCount,
		Sessions:               sessions,
		CreatedAt:              o.CreatedAt,
		UpdatedAt:              o.UpdatedAt,
	}
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

// GetEventStats godoc
// @Summary      Get event statistics
// @Description  Returns stats for a specific event: comments, carts (open/paid), products sold, revenue (projected/confirmed)
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=EventStatsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/event-stats [get]
// @Security     BearerAuth
func (h *Handler) GetEventStats(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	output, err := h.service.GetEventStats(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, EventStatsResponse{
		TotalComments:     output.TotalComments,
		TotalCarts:        output.TotalCarts,
		OpenCarts:         output.OpenCarts,
		CheckoutCarts:     output.CheckoutCarts,
		PaidCarts:         output.PaidCarts,
		TotalProductsSold: output.TotalProductsSold,
		ProjectedRevenue:  output.ProjectedRevenue,
		ConfirmedRevenue:  output.ConfirmedRevenue,
	})
}

// ListCarts godoc
// @Summary      List carts for an event
// @Description  Returns all carts for an event with total value and item count
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=ListCartsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/carts [get]
// @Security     BearerAuth
func (h *Handler) ListCarts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	carts, err := h.service.ListCartsWithTotalByEvent(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]CartWithTotalResponse, len(carts))
	for i, cart := range carts {
		responses[i] = CartWithTotalResponse{
			ID:              cart.ID,
			SessionID:       cart.SessionID,
			PlatformUserID:  cart.PlatformUserID,
			PlatformHandle:  cart.PlatformHandle,
			Status:          cart.Status,
			PaymentStatus:   cart.PaymentStatus,
			TotalValue:      cart.TotalValue,
			TotalItems:      cart.TotalItems,
			AvailableItems:  cart.AvailableItems,
			WaitlistedItems: cart.WaitlistedItems,
			CreatedAt:       cart.CreatedAt,
			ExpiresAt:       cart.ExpiresAt,
		}
	}

	return httpx.OK(c, ListCartsResponse{Data: responses})
}

// ListProducts godoc
// @Summary      List products sold in an event
// @Description  Returns all products sold in an event with quantity and revenue
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live event UUID"
// @Success      200 {object} httpx.Envelope{data=ListEventProductSalesResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/products [get]
// @Security     BearerAuth
func (h *Handler) ListProducts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	products, err := h.service.ListProductsByEvent(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]EventProductSalesResponse, len(products))
	for i, product := range products {
		responses[i] = EventProductSalesResponse{
			ID:            product.ID,
			Name:          product.Name,
			ImageURL:      product.ImageURL,
			Keyword:       product.Keyword,
			TotalQuantity: product.TotalQuantity,
			TotalRevenue:  product.TotalRevenue,
		}
	}

	return httpx.OK(c, ListEventProductSalesResponse{Data: responses})
}

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// GetLiveModeState godoc
// @Summary      Get live mode state
// @Description  Returns the current active product and processing paused state
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/live-mode [get]
// @Security     BearerAuth
func (h *Handler) GetLiveModeState(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	state, err := h.service.GetLiveModeState(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveModeStateResponse(state))
}

// SetActiveProduct godoc
// @Summary      Set active product for live mode
// @Description  Sets the active product that will be used as fallback for comments without keywords
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        request body SetActiveProductRequest true "Active product"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/active-product [patch]
// @Security     BearerAuth
func (h *Handler) SetActiveProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req SetActiveProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	state, err := h.service.SetActiveProduct(c.Context(), eventID, storeID, req.ProductID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveModeStateResponse(state))
}

// SetProcessingPaused godoc
// @Summary      Pause or resume comment processing
// @Description  When paused, comments are stored but not processed into carts
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        request body SetProcessingPausedRequest true "Processing state"
// @Success      200 {object} httpx.Envelope{data=LiveModeStateResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/pause-processing [patch]
// @Security     BearerAuth
func (h *Handler) SetProcessingPaused(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req SetProcessingPausedRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	state, err := h.service.SetProcessingPaused(c.Context(), eventID, storeID, req.Paused)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveModeStateResponse(state))
}

func toLiveModeStateResponse(state *LiveModeStateOutput) LiveModeStateResponse {
	resp := LiveModeStateResponse{
		ProcessingPaused: state.ProcessingPaused,
	}

	if state.ActiveProduct != nil {
		resp.ActiveProduct = &ActiveProductResponse{
			ID:       state.ActiveProduct.ID,
			Name:     state.ActiveProduct.Name,
			Keyword:  state.ActiveProduct.Keyword,
			Price:    state.ActiveProduct.Price,
			ImageURL: state.ActiveProduct.ImageURL,
		}
	}

	return resp
}

// =============================================================================
// EVENT PRODUCTS (Whitelist)
// =============================================================================

// ListEventProducts godoc
// @Summary      List event products in whitelist
// @Description  Returns all products configured for this event's whitelist
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Success      200 {object} httpx.Envelope{data=ListEventProductsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/whitelist [get]
// @Security     BearerAuth
func (h *Handler) ListEventProducts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	products, err := h.service.ListEventProducts(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]EventProductResponse, len(products))
	for i, p := range products {
		responses[i] = toEventProductResponse(p)
	}

	return httpx.OK(c, ListEventProductsResponse{Data: responses})
}

// AddEventProduct godoc
// @Summary      Add product to event whitelist
// @Description  Adds a product with optional special price and max quantity
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        request body EventProductRequest true "Product configuration"
// @Success      201 {object} httpx.Envelope{data=EventProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/whitelist [post]
// @Security     BearerAuth
func (h *Handler) AddEventProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req EventProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.AddEventProduct(c.Context(), AddEventProductInput{
		EventID:      eventID,
		StoreID:      storeID,
		ProductID:    req.ProductID,
		SpecialPrice: req.SpecialPrice,
		MaxQuantity:  req.MaxQuantity,
		DisplayOrder: req.DisplayOrder,
		Featured:     req.Featured,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, toEventProductResponse(output))
}

// UpdateEventProduct godoc
// @Summary      Update event product configuration
// @Description  Updates special price, max quantity, display order, or featured status
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        productId path string true "Event Product UUID"
// @Param        request body EventProductRequest true "Product configuration"
// @Success      200 {object} httpx.Envelope{data=EventProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/whitelist/{productId} [put]
// @Security     BearerAuth
func (h *Handler) UpdateEventProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")
	productID := c.Params("productId")

	var req EventProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	output, err := h.service.UpdateEventProduct(c.Context(), UpdateEventProductInput{
		ID:           productID,
		EventID:      eventID,
		StoreID:      storeID,
		SpecialPrice: req.SpecialPrice,
		MaxQuantity:  req.MaxQuantity,
		DisplayOrder: req.DisplayOrder,
		Featured:     req.Featured,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toEventProductResponse(output))
}

// DeleteEventProduct godoc
// @Summary      Remove product from event whitelist
// @Description  Removes a product from the event's whitelist
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        productId path string true "Event Product UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/whitelist/{productId} [delete]
// @Security     BearerAuth
func (h *Handler) DeleteEventProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")
	productID := c.Params("productId")

	if err := h.service.DeleteEventProduct(c.Context(), productID, eventID, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, productID)
}

func toEventProductResponse(o EventProductOutput) EventProductResponse {
	return EventProductResponse{
		ID:             o.ID,
		ProductID:      o.ProductID,
		Name:           o.Name,
		Keyword:        o.Keyword,
		ImageURL:       o.ImageURL,
		OriginalPrice:  o.OriginalPrice,
		SpecialPrice:   o.SpecialPrice,
		EffectivePrice: o.EffectivePrice,
		MaxQuantity:    o.MaxQuantity,
		DisplayOrder:   o.DisplayOrder,
		Featured:       o.Featured,
		Stock:          o.Stock,
		ProductActive:  o.ProductActive,
	}
}

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// ListEventUpsells godoc
// @Summary      List event upsells
// @Description  Returns all upsells configured for this event
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Success      200 {object} httpx.Envelope{data=ListEventUpsellsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/upsells [get]
// @Security     BearerAuth
func (h *Handler) ListEventUpsells(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	upsells, err := h.service.ListEventUpsells(c.Context(), eventID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]EventUpsellResponse, len(upsells))
	for i, u := range upsells {
		responses[i] = toEventUpsellResponse(u)
	}

	return httpx.OK(c, ListEventUpsellsResponse{Data: responses})
}

// AddEventUpsell godoc
// @Summary      Add upsell to event
// @Description  Adds a product as an upsell with discount percentage
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        request body EventUpsellRequest true "Upsell configuration"
// @Success      201 {object} httpx.Envelope{data=EventUpsellResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/upsells [post]
// @Security     BearerAuth
func (h *Handler) AddEventUpsell(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")

	var req EventUpsellRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.AddEventUpsell(c.Context(), AddEventUpsellInput{
		EventID:         eventID,
		StoreID:         storeID,
		ProductID:       req.ProductID,
		DiscountPercent: req.DiscountPercent,
		MessageTemplate: req.MessageTemplate,
		DisplayOrder:    req.DisplayOrder,
		Active:          req.Active,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, toEventUpsellResponse(output))
}

// UpdateEventUpsell godoc
// @Summary      Update event upsell
// @Description  Updates discount percent, message template, display order, or active status
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        upsellId path string true "Upsell UUID"
// @Param        request body EventUpsellRequest true "Upsell configuration"
// @Success      200 {object} httpx.Envelope{data=EventUpsellResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/upsells/{upsellId} [put]
// @Security     BearerAuth
func (h *Handler) UpdateEventUpsell(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")
	upsellID := c.Params("upsellId")

	var req EventUpsellRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	output, err := h.service.UpdateEventUpsell(c.Context(), UpdateEventUpsellInput{
		ID:              upsellID,
		EventID:         eventID,
		StoreID:         storeID,
		DiscountPercent: req.DiscountPercent,
		MessageTemplate: req.MessageTemplate,
		DisplayOrder:    req.DisplayOrder,
		Active:          req.Active,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toEventUpsellResponse(output))
}

// DeleteEventUpsell godoc
// @Summary      Remove upsell from event
// @Description  Removes an upsell from the event
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Event UUID"
// @Param        upsellId path string true "Upsell UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/upsells/{upsellId} [delete]
// @Security     BearerAuth
func (h *Handler) DeleteEventUpsell(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	eventID := c.Params("id")
	upsellID := c.Params("upsellId")

	if err := h.service.DeleteEventUpsell(c.Context(), upsellID, eventID, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, upsellID)
}

func toEventUpsellResponse(o EventUpsellOutput) EventUpsellResponse {
	return EventUpsellResponse{
		ID:              o.ID,
		ProductID:       o.ProductID,
		Name:            o.Name,
		Keyword:         o.Keyword,
		ImageURL:        o.ImageURL,
		OriginalPrice:   o.OriginalPrice,
		DiscountPercent: o.DiscountPercent,
		DiscountedPrice: o.DiscountedPrice,
		MessageTemplate: o.MessageTemplate,
		DisplayOrder:    o.DisplayOrder,
		Active:          o.Active,
		Stock:           o.Stock,
	}
}
