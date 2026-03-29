package live

import (
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

	// Platform aggregation
	g.Get("/:id/platforms", h.ListPlatforms)
	g.Post("/:id/platforms", h.AddPlatform)
	g.Delete("/:id/platforms/:platformLiveId", h.RemovePlatform)
}

// Create godoc
// @Summary      Create a new live session
// @Description  Creates a live session for the current store
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateLiveRequest true "Live session creation payload"
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

	output, err := h.service.Create(c.Context(), CreateLiveInput{
		StoreID:        storeID,
		Title:          req.Title,
		Platform:       req.Platform,
		PlatformLiveID: req.PlatformLiveID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CreateLiveResponse{
		ID:        output.ID,
		Title:     output.Title,
		Platform:  output.Platform,
		Status:    output.Status,
		CreatedAt: output.CreatedAt,
	})
}

// GetByID godoc
// @Summary      Get live session by ID
// @Description  Returns a single live session by its UUID
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
// @Success      200 {object} httpx.Envelope{data=LiveResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveResponse(output))
}

// List godoc
// @Summary      List live sessions
// @Description  Returns live sessions with filtering, pagination, and sorting
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by title"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by status"
// @Param        platform query []string false "Filter by platform"
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
// @Summary      Delete a live session
// @Description  Deletes a live session by its UUID
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
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
// @Summary      Update a live session
// @Description  Updates an existing live session by its UUID
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
// @Param        request body UpdateLiveRequest true "Live session update payload"
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
		StoreID:        storeID,
		ID:             id,
		Title:          req.Title,
		Platform:       req.Platform,
		PlatformLiveID: req.PlatformLiveID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toLiveResponse(output))
}

// Start godoc
// @Summary      Start a live session
// @Description  Starts a scheduled live session
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
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
// @Summary      End a live session
// @Description  Ends an active live session and finalizes all pending carts
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
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
		AutoSend: req.AutoSendCheckoutLinks,
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
// @Description  Returns aggregated statistics for all live sessions in the store
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
		ID:             o.ID,
		Title:          o.Title,
		Platform:       o.Platform,
		PlatformLiveID: o.PlatformLiveID,
		Status:         o.Status,
		StartedAt:      o.StartedAt,
		EndedAt:        o.EndedAt,
		TotalComments:  o.TotalComments,
		TotalOrders:    o.TotalOrders,
		CreatedAt:      o.CreatedAt,
		UpdatedAt:      o.UpdatedAt,
	}
}

// =============================================================================
// PLATFORM AGGREGATION
// =============================================================================

// ListPlatforms godoc
// @Summary      List platforms for a live session
// @Description  Returns all platform IDs associated with a live session
// @Tags         lives
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
// @Success      200 {object} httpx.Envelope{data=ListPlatformsResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms [get]
// @Security     BearerAuth
func (h *Handler) ListPlatforms(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	sessionID := c.Params("id")

	platforms, err := h.service.ListPlatforms(c.Context(), sessionID, storeID)
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
// @Summary      Add a platform to a live session
// @Description  Associates a new platform live ID with the session (for crash recovery)
// @Tags         lives
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
// @Param        request body AddPlatformRequest true "Platform to add"
// @Success      201 {object} httpx.Envelope{data=PlatformResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms [post]
// @Security     BearerAuth
func (h *Handler) AddPlatform(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	sessionID := c.Params("id")

	var req AddPlatformRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Get session to determine platform type
	session, err := h.service.GetByID(c.Context(), sessionID, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	output, err := h.service.AddPlatform(c.Context(), AddPlatformInput{
		SessionID:      sessionID,
		StoreID:        storeID,
		Platform:       session.Platform,
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
// @Summary      Remove a platform from a live session
// @Description  Disassociates a platform live ID from the session
// @Tags         lives
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Live session UUID"
// @Param        platformLiveId path string true "Platform live ID to remove"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/lives/{id}/platforms/{platformLiveId} [delete]
// @Security     BearerAuth
func (h *Handler) RemovePlatform(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	sessionID := c.Params("id")
	platformLiveID := c.Params("platformLiveId")

	if err := h.service.RemovePlatform(c.Context(), sessionID, storeID, platformLiveID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, platformLiveID)
}
