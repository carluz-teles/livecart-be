package integration

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
	"livecart/apps/api/lib/storage"
)

// Handler handles HTTP requests for integrations.
type Handler struct {
	service  *Service
	validate *validator.Validate
	s3Client *storage.S3Client
}

// NewHandler creates a new integration handler.
func NewHandler(service *Service, validate *validator.Validate, s3Client *storage.S3Client) *Handler {
	return &Handler{
		service:  service,
		validate: validate,
		s3Client: s3Client,
	}
}

// RegisterRoutes registers the integration routes.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/integrations")

	// CRUD
	g.Get("/", h.List)
	g.Post("/", h.Create)

	// Provider setup URLs (must be registered before /:id so it's not eaten by
	// the wildcard).
	g.Get("/providers/:provider/urls", h.GetProviderURLs)

	g.Get("/:id", h.GetByID)
	g.Delete("/:id", h.Delete)
	g.Patch("/:id/priority", h.UpdatePriority)

	// Test connection
	g.Post("/:id/test", h.TestConnection)

	// ERP cadastro audit (Tiny v3): checks formas-pagamento /
	// formas-recebimento / formas-envio against the canonical names so
	// the merchant sees what to register before the first sale.
	g.Get("/:id/erp/health-check", h.RunERPHealthCheck)

	// Instagram operations
	g.Get("/instagram/lives", h.GetInstagramLives)
	g.Get("/instagram/media", h.GetInstagramMedia)

	// Instagram content publishing (create a post in LiveCart)
	g.Post("/instagram/media/upload", h.UploadInstagramMedia)
	g.Post("/instagram/posts", h.CreateInstagramPost)
	g.Post("/instagram/reels", h.CreateInstagramReel)
	g.Post("/instagram/stories", h.CreateInstagramStory)

	// Instagram comment moderation (reply / hide / delete)
	g.Post("/instagram/comments/:commentId/reply", h.ReplyInstagramComment)
	g.Post("/instagram/comments/:commentId/hide", h.HideInstagramComment)
	g.Delete("/instagram/comments/:commentId", h.DeleteInstagramComment)

	// OAuth connect
	g.Get("/oauth/:provider/connect", h.OAuthConnect)

	// WhatsApp (Twilio) — PRD 006. Onboarding (subaccount + sender OTP +
	// default template), status polling and test send.
	g.Post("/whatsapp/connect", h.ConnectWhatsApp)
	g.Post("/whatsapp/verify", h.VerifyWhatsApp)
	g.Get("/whatsapp/status", h.GetWhatsAppStatus)
	g.Post("/whatsapp/test-message", h.SendWhatsAppTestMessage)

	// Payment — provider-specific connect (no OAuth). Pagar.me uses static
	// API keys (sk_*/pk_*), so the merchant pastes them into a form and we
	// validate live against the gateway before persisting.
	g.Post("/payment/pagarme/connect", h.ConnectPagarme)
	g.Get("/:id/pagarme/webhook-status", h.GetPagarmeWebhookStatus)

	// Shipping — token-based connect (no OAuth) + order lifecycle helpers.
	// These are typed at the provider level because each shipping provider
	// has its own auth model; routing by :provider keeps a single surface.
	g.Post("/shipping/smartenvios/connect", h.ConnectSmartEnvios)
	g.Get("/shipping/:provider/carriers", h.ListShippingCarriers)
	g.Post("/shipping/:provider/shipments", h.CreateShippingShipment)
	g.Post("/shipping/:provider/shipments/:shipmentId/invoice", h.AttachShippingInvoice)
	g.Post("/shipping/:provider/shipments/:shipmentId/invoice-xml", h.UploadShippingInvoiceXML)
	g.Post("/shipping/:provider/labels", h.GenerateShippingLabels)
	g.Post("/shipping/:provider/tracking", h.TrackShipping)

	// ERP operations
	g.Get("/:id/products", h.SearchProducts)
	g.Post("/:id/products/:productId/sync", h.SyncProduct)
	g.Post("/:id/products/:tinyProductId/import", h.ImportERPProduct)

	// Payment operations (Mercado Pago)
	g.Post("/:id/checkout", h.CreateCheckout)
	g.Get("/:id/payments/:paymentId", h.GetPaymentStatus)
	g.Post("/:id/payments/:paymentId/refund", h.RefundPayment)
}

// =============================================================================
// CRUD HANDLERS
// =============================================================================

// Create creates a new integration.
// @Summary Create integration
// @Description Creates a new external service integration
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param body body CreateIntegrationRequest true "Integration data"
// @Success 201 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/integrations [post]
// @Security BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req CreateIntegrationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	// Convert credentials map to Credentials struct
	creds := mapToCredentials(req.Credentials)

	output, err := h.service.Create(c.Context(), CreateIntegrationInput{
		StoreID:     storeID,
		Type:        req.Type,
		Provider:    req.Provider,
		Credentials: creds,
		Metadata:    req.Metadata,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, toIntegrationResponse(output))
}

// List lists all integrations for a store.
// @Summary List integrations
// @Description Lists all integrations for the current store
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Success 200 {object} httpx.Envelope{data=ListIntegrationsResponse}
// @Router /api/v1/stores/{storeId}/integrations [get]
// @Security BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	input := ListIntegrationsInput{
		StoreID: storeID,
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
	}

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	integrations := make([]IntegrationResponse, len(output.Integrations))
	for i, o := range output.Integrations {
		integrations[i] = *toIntegrationResponse(&o)
	}

	return httpx.OK(c, ListIntegrationsResponse{
		Data:       integrations,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

// GetByID retrieves an integration by ID.
// @Summary Get integration
// @Description Retrieves an integration by ID
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=IntegrationResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id} [get]
// @Security BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toIntegrationResponse(output))
}

// Delete deletes an integration.
// @Summary Delete integration
// @Description Deletes an integration
// @Tags integrations
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id} [delete]
// @Security BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	if err := h.service.Delete(c.Context(), id, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, id)
}

// UpdatePriority sets the priority of a single integration. Lower number =
// higher priority in the checkout selection ordering. The admin UI uses this
// to reorder multiple payment providers connected to the same store.
// @Summary Update integration priority
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param body body UpdatePriorityRequest true "Priority"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/priority [patch]
// @Security BearerAuth
func (h *Handler) UpdatePriority(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	var req UpdatePriorityRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.HandleServiceError(c, httpx.ErrBadRequest("payload inválido"))
	}
	if req.Priority < 0 {
		return httpx.HandleServiceError(c, httpx.ErrBadRequest("priority deve ser >= 0"))
	}

	if err := h.service.UpdatePriority(c.Context(), id, storeID, req.Priority); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, fiber.Map{"id": id, "priority": req.Priority})
}

// TestConnection tests the connection to the integration provider.
// @Summary Test integration connection
// @Description Tests if the integration credentials are valid and the provider is reachable
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=TestConnectionResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/test [post]
// @Security BearerAuth
func (h *Handler) TestConnection(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.TestConnection(c.Context(), TestConnectionInput{
		StoreID:       storeID,
		IntegrationID: id,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, TestConnectionResponse{
		Success:     output.Success,
		Message:     output.Message,
		LatencyMs:   output.Latency.Milliseconds(),
		AccountInfo: output.AccountInfo,
		TestedAt:    output.TestedAt,
	})
}

// RunERPHealthCheck audits the merchant's ERP cadastros (formas de pagamento,
// recebimento e envio) against what LiveCart expects when criando pedidos.
// Returns supported=false when the underlying ERP provider doesn't expose
// the audit capability — the frontend should hide the section in that case.
// @Summary ERP cadastros health check
// @Description Audits ERP cadastros (formas-pagamento / recebimento / envio) against LiveCart canonical names
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Success 200 {object} httpx.Envelope{data=ERPHealthCheckResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/erp/health-check [get]
// @Security BearerAuth
func (h *Handler) RunERPHealthCheck(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.RunERPHealthCheck(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

// =============================================================================
// INSTAGRAM HANDLERS
// =============================================================================

// GetInstagramLives retrieves all active Instagram lives for the store.
// @Summary Get active Instagram lives
// @Description Returns all live videos currently being broadcast on the connected Instagram account
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 200 {object} httpx.Envelope{data=InstagramLivesResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/lives [get]
// @Security BearerAuth
func (h *Handler) GetInstagramLives(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	lives, err := h.service.FetchInstagramLives(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"data": lives})
}

// GetInstagramMedia lists recent posts/reels for the post-event selector.
// @Summary Get recent Instagram posts
// @Description Returns recent published posts/reels of the connected Instagram account
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param limit query int false "Max items" default(25)
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/media [get]
// @Security BearerAuth
func (h *Handler) GetInstagramMedia(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	page, err := h.service.FetchInstagramMedia(c.Context(), storeID, c.QueryInt("limit", 24), c.Query("after"))
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"data": page.Posts, "after": page.After})
}

// UploadInstagramMedia uploads a JPEG image to storage and returns a public URL
// Instagram can fetch during content publishing. The image is transient — once
// the post is published, the event references the Instagram media itself.
// @Summary Upload an image for an Instagram post
// @Tags integrations
// @Accept multipart/form-data
// @Produce json
// @Param storeId path string true "Store ID"
// @Param file formData file true "JPEG image (max 8MB)"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/media/upload [post]
// @Security BearerAuth
func (h *Handler) UploadInstagramMedia(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	if h.s3Client == nil {
		return httpx.InternalError(c, "storage not configured")
	}

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.BadRequest(c, "file is required")
	}
	// Instagram content publishing accepts JPEG images up to 8MB.
	if file.Size > 8*1024*1024 {
		return httpx.BadRequest(c, "file too large, maximum size is 8MB")
	}
	contentType := file.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/jpg" {
		return httpx.BadRequest(c, "invalid file type — Instagram requires a JPEG image")
	}

	src, err := file.Open()
	if err != nil {
		return httpx.InternalError(c, "failed to read file")
	}
	defer src.Close()

	key, err := h.s3Client.UploadFile(c.Context(), src, file.Filename, contentType, "instagram/"+storeID)
	if err != nil {
		return httpx.InternalError(c, "failed to upload file")
	}

	// A short-lived presigned GET URL is enough: Instagram fetches the image at
	// publish time (moments after the merchant submits), and we delete the
	// object right after publishing. The key is returned so the publish step
	// can remove it.
	url, err := h.s3Client.GeneratePresignedGetURL(c.Context(), key, 2*time.Hour)
	if err != nil {
		return httpx.InternalError(c, "failed to generate file URL")
	}

	return httpx.OK(c, map[string]any{"url": url, "key": key})
}

// CreateInstagramPost publishes an image post on the connected Instagram account
// and creates a post-commerce event bound to the new post in one step.
// @Summary Create an Instagram post and its post event
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Success 201 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/posts [post]
// @Security BearerAuth
func (h *Handler) CreateInstagramPost(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	var req CreateInstagramPostRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	var startsAt, endsAt *time.Time
	if req.StartsAt != nil && *req.StartsAt != "" {
		t, err := time.Parse(time.RFC3339, *req.StartsAt)
		if err != nil {
			return httpx.BadRequest(c, "invalid startsAt format")
		}
		startsAt = &t
	}
	if req.EndsAt != nil && *req.EndsAt != "" {
		t, err := time.Parse(time.RFC3339, *req.EndsAt)
		if err != nil {
			return httpx.BadRequest(c, "invalid endsAt format")
		}
		endsAt = &t
	}
	if startsAt != nil && endsAt != nil && !endsAt.After(*startsAt) {
		return httpx.BadRequest(c, "endsAt must be after startsAt")
	}

	event, err := h.service.CreateInstagramPostEvent(c.Context(), CreateInstagramPostInput{
		StoreID:                storeID,
		ImageURL:               req.ImageURL,
		ImageKey:               req.ImageKey, // deleted (with logging) after publish
		Caption:                req.Caption,
		Title:                  req.Title,
		ProductIDs:             req.ProductIDs,
		StartsAt:               startsAt,
		EndsAt:                 endsAt,
		CartExpirationMinutes:  req.CartExpirationMinutes,
		CartMaxQuantityPerItem: req.CartMaxQuantityPerItem,
		IdempotencyKey:         derefString(req.IdempotencyKey),
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, event)
}

// CreateInstagramReel uploads a video as a Reel (streamed directly to Instagram,
// no hosting), publishes it, and creates the bound post event. Multipart: the
// video file plus the same fields as a post (caption, products, window).
// @Summary Create an Instagram Reel and its post event
// @Tags integrations
// @Accept multipart/form-data
// @Produce json
// @Param storeId path string true "Store ID"
// @Param file formData file true "Video (MP4, max 300MB)"
// @Success 201 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/reels [post]
// @Security BearerAuth
func (h *Handler) CreateInstagramReel(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.BadRequest(c, "file is required")
	}
	if file.Size > 300*1024*1024 {
		return httpx.BadRequest(c, "video too large, maximum size is 300MB")
	}
	ct := file.Header.Get("Content-Type")
	if ct != "video/mp4" && ct != "video/quicktime" {
		return httpx.BadRequest(c, "invalid file type — Instagram Reels require an MP4 video")
	}

	// productIds is sent as a JSON array string in the multipart form.
	var productIDs []string
	if raw := c.FormValue("productIds"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &productIDs); err != nil {
			return httpx.BadRequest(c, "invalid productIds")
		}
	}
	if len(productIDs) == 0 {
		return httpx.BadRequest(c, "select at least one product for the promotion")
	}

	startsAt, endsAt, perr := parseOptionalWindow(c.FormValue("startsAt"), c.FormValue("endsAt"))
	if perr != nil {
		return httpx.BadRequest(c, perr.Error())
	}

	var cartExp, maxQty *int
	if v := c.FormValue("cartExpirationMinutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cartExp = &n
		}
	}
	if v := c.FormValue("cartMaxQuantityPerItem"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxQty = &n
		}
	}

	if h.s3Client == nil {
		return httpx.InternalError(c, "storage not configured")
	}
	src, err := file.Open()
	if err != nil {
		return httpx.InternalError(c, "failed to read file")
	}
	defer src.Close()

	// Instagram fetches the video from a public URL (it does not accept the bytes
	// on this API), so host it transiently and pass a presigned URL. The service
	// deletes the object after publishing.
	key, err := h.s3Client.UploadFile(c.Context(), src, file.Filename, ct, "instagram/"+storeID)
	if err != nil {
		return httpx.InternalError(c, "failed to upload video")
	}
	videoURL, err := h.s3Client.GeneratePresignedGetURL(c.Context(), key, 6*time.Hour)
	if err != nil {
		return httpx.InternalError(c, "failed to generate video URL")
	}

	event, err := h.service.CreateInstagramReelEvent(c.Context(), CreateInstagramPostInput{
		StoreID:                storeID,
		ImageKey:               key, // the transient video key, deleted after publish
		Caption:                c.FormValue("caption"),
		Title:                  c.FormValue("title"),
		ProductIDs:             productIDs,
		StartsAt:               startsAt,
		EndsAt:                 endsAt,
		CartExpirationMinutes:  cartExp,
		CartMaxQuantityPerItem: maxQty,
		IdempotencyKey:         c.FormValue("idempotencyKey"),
	}, videoURL)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, event)
}

// CreateInstagramStory publishes a Story (photo or video) and creates the bound
// story-commerce event (24h window, purchase intent captured via DM replies).
// Multipart: the media file plus products + cart settings (no start/end window —
// Stories always run for their 24h lifetime).
// @Summary Create an Instagram Story and its event
// @Tags integrations
// @Accept multipart/form-data
// @Produce json
// @Param storeId path string true "Store ID"
// @Param file formData file true "Photo (JPEG) or video (MP4)"
// @Success 201 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/stories [post]
// @Security BearerAuth
func (h *Handler) CreateInstagramStory(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.BadRequest(c, "file is required")
	}
	ct := file.Header.Get("Content-Type")
	var isVideo bool
	switch ct {
	case "image/jpeg":
		isVideo = false
		if file.Size > 8*1024*1024 {
			return httpx.BadRequest(c, "image too large, maximum size is 8MB")
		}
	case "video/mp4", "video/quicktime":
		isVideo = true
		if file.Size > 100*1024*1024 {
			return httpx.BadRequest(c, "video too large, maximum size is 100MB")
		}
	default:
		return httpx.BadRequest(c, "invalid file type — send a JPEG photo or an MP4 video")
	}

	var productIDs []string
	if raw := c.FormValue("productIds"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &productIDs); err != nil {
			return httpx.BadRequest(c, "invalid productIds")
		}
	}
	if len(productIDs) == 0 {
		return httpx.BadRequest(c, "select at least one product for the promotion")
	}

	var cartExp, maxQty *int
	if v := c.FormValue("cartExpirationMinutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cartExp = &n
		}
	}
	if v := c.FormValue("cartMaxQuantityPerItem"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxQty = &n
		}
	}

	if h.s3Client == nil {
		return httpx.InternalError(c, "storage not configured")
	}
	src, err := file.Open()
	if err != nil {
		return httpx.InternalError(c, "failed to read file")
	}
	defer src.Close()

	// Instagram fetches the media from a public URL, so host it transiently and
	// pass a presigned URL. The service deletes the object after publishing.
	key, err := h.s3Client.UploadFile(c.Context(), src, file.Filename, ct, "instagram/"+storeID)
	if err != nil {
		return httpx.InternalError(c, "failed to upload media")
	}
	mediaURL, err := h.s3Client.GeneratePresignedGetURL(c.Context(), key, 6*time.Hour)
	if err != nil {
		return httpx.InternalError(c, "failed to generate media URL")
	}

	event, err := h.service.CreateInstagramStoryEvent(c.Context(), CreateInstagramPostInput{
		StoreID:                storeID,
		ImageKey:               key, // transient media key, deleted after publish
		Title:                  c.FormValue("title"),
		ProductIDs:             productIDs,
		CartExpirationMinutes:  cartExp,
		CartMaxQuantityPerItem: maxQty,
		IdempotencyKey:         c.FormValue("idempotencyKey"),
	}, mediaURL, isVideo)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, event)
}

// derefString returns the pointed-to string, or "" when the pointer is nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseOptionalWindow parses optional RFC3339 start/end and validates ordering.
func parseOptionalWindow(startRaw, endRaw string) (*time.Time, *time.Time, error) {
	var startsAt, endsAt *time.Time
	if startRaw != "" {
		t, err := time.Parse(time.RFC3339, startRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid startsAt format")
		}
		startsAt = &t
	}
	if endRaw != "" {
		t, err := time.Parse(time.RFC3339, endRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid endsAt format")
		}
		endsAt = &t
	}
	if startsAt != nil && endsAt != nil && !endsAt.After(*startsAt) {
		return nil, nil, fmt.Errorf("endsAt must be after startsAt")
	}
	return startsAt, endsAt, nil
}

// ReplyInstagramComment posts a public reply comment to a live/post comment.
// @Summary Reply to an Instagram comment (public)
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param commentId path string true "Instagram comment ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/comments/{commentId}/reply [post]
// @Security BearerAuth
func (h *Handler) ReplyInstagramComment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	commentID := c.Params("commentId")

	var req struct {
		Text string `json:"text"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if req.Text == "" {
		return httpx.BadRequest(c, "text is required")
	}

	if err := h.service.PublicReplyToInstagramComment(c.Context(), storeID, commentID, req.Text); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"commentId": commentID, "replied": true})
}

// HideInstagramComment hides or unhides an Instagram comment.
// @Summary Hide/unhide an Instagram comment
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param commentId path string true "Instagram comment ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/comments/{commentId}/hide [post]
// @Security BearerAuth
func (h *Handler) HideInstagramComment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	commentID := c.Params("commentId")

	var req struct {
		Hidden bool `json:"hidden"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	if err := h.service.HideInstagramComment(c.Context(), storeID, commentID, req.Hidden); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"commentId": commentID, "hidden": req.Hidden})
}

// DeleteInstagramComment deletes an Instagram comment.
// @Summary Delete an Instagram comment
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param commentId path string true "Instagram comment ID"
// @Success 200 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/instagram/comments/{commentId} [delete]
// @Security BearerAuth
func (h *Handler) DeleteInstagramComment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	commentID := c.Params("commentId")

	if err := h.service.DeleteInstagramComment(c.Context(), storeID, commentID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, map[string]any{"commentId": commentID, "deleted": true})
}

// =============================================================================
// ERP HANDLERS
// =============================================================================

// SearchProducts searches for products in an ERP integration.
// @Summary Search ERP products
// @Description Searches for products in an ERP integration by name, SKU, or barcode
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param search query string true "Search term (product name, SKU, or barcode)"
// @Param limit query int false "Max results" default(20)
// @Success 200 {object} httpx.Envelope{data=SearchProductsOutput}
// @Failure 400 {object} httpx.Envelope
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products [get]
// @Security BearerAuth
func (h *Handler) SearchProducts(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	search := c.Query("search")

	if search == "" {
		return httpx.BadRequest(c, "search parameter is required")
	}

	output, err := h.service.SearchProducts(c.Context(), SearchProductsInput{
		StoreID:       storeID,
		IntegrationID: id,
		Search:        search,
		PageSize:      c.QueryInt("limit", 20),
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

// ImportERPProduct imports a product (or a subset of its variations) from the
// ERP into the LiveCart catalog. If the product has no variations, a single
// simple product is created. If it has variations, a product_group with N
// variants is created in one transaction.
//
// Body:
//
//	{ "variantIds": ["67890", "67891"] }   // optional subset
//	{ }                                    // import everything
//
// @Summary Import product from ERP into LiveCart catalog
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param tinyProductId path string true "ERP product ID (Tiny parent or simple)"
// @Param request body ImportERPProductRequest false "Optional subset of variant IDs"
// @Success 201 {object} httpx.Envelope{data=ImportERPProductOutput}
// @Failure 404 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products/{tinyProductId}/import [post]
// @Security BearerAuth
func (h *Handler) ImportERPProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")
	tinyProductID := c.Params("tinyProductId")

	var req ImportERPProductRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return httpx.BadRequest(c, "invalid request body")
		}
		if err := h.validate.Struct(req); err != nil {
			return httpx.ValidationError(c, err)
		}
	}

	output, err := h.service.ImportERPProduct(c.Context(), ImportERPProductInput{
		StoreID:       storeID,
		IntegrationID: integrationID,
		TinyProductID: tinyProductID,
		VariantIDs:    req.VariantIDs,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, output)
}

// SyncProduct manually syncs a product from the ERP to LiveCart.
// @Summary Sync product from ERP
// @Description Fetches the latest product data from the ERP and updates the local product
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param productId path string true "Product ID (LiveCart)"
// @Success 200 {object} httpx.Envelope{data=SyncProductOutput}
// @Failure 404 {object} httpx.Envelope
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/products/{productId}/sync [post]
// @Security BearerAuth
func (h *Handler) SyncProduct(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	integrationID := c.Params("id")
	productID := c.Params("productId")

	output, err := h.service.SyncProductManual(c.Context(), SyncProductInput{
		StoreID:       storeID,
		IntegrationID: integrationID,
		ProductID:     productID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, output)
}

// =============================================================================
// PAYMENT HANDLERS
// =============================================================================

// CreateCheckout creates a payment checkout session.
// @Summary Create checkout
// @Description Creates a payment checkout session
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param X-Idempotency-Key header string false "Idempotency key"
// @Param body body CreateCheckoutRequest true "Checkout data"
// @Success 201 {object} httpx.Envelope{data=CheckoutResponse}
// @Failure 400 {object} httpx.Envelope
// @Failure 422 {object} httpx.ValidationEnvelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/checkout [post]
// @Security BearerAuth
func (h *Handler) CreateCheckout(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	idempotencyKey := c.Get("X-Idempotency-Key")

	var req CreateCheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	// Override integration ID from path
	req.IntegrationID = id

	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.CreateCheckout(c.Context(), CreateCheckoutInput{
		StoreID:        storeID,
		IntegrationID:  id,
		IdempotencyKey: idempotencyKey,
		CartID:         req.CartID,
		Items:          req.Items,
		Customer:       req.Customer,
		TotalAmount:    req.TotalAmount,
		Currency:       req.Currency,
		SuccessURL:     req.SuccessURL,
		FailureURL:     req.FailureURL,
		Metadata:       req.Metadata,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CheckoutResponse{
		CheckoutID:  output.CheckoutID,
		CheckoutURL: output.CheckoutURL,
		ExpiresAt:   output.ExpiresAt,
	})
}

// GetPaymentStatus retrieves the status of a payment.
// @Summary Get payment status
// @Description Retrieves the current status of a payment
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param paymentId path string true "Payment ID"
// @Success 200 {object} httpx.Envelope{data=PaymentStatusResponse}
// @Failure 404 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/payments/{paymentId} [get]
// @Security BearerAuth
func (h *Handler) GetPaymentStatus(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	paymentID := c.Params("paymentId")

	output, err := h.service.GetPaymentStatus(c.Context(), GetPaymentStatusInput{
		StoreID:       storeID,
		IntegrationID: id,
		PaymentID:     paymentID,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, PaymentStatusResponse{
		PaymentID:     output.PaymentID,
		Status:        output.Status,
		Amount:        output.Amount,
		PaidAt:        output.PaidAt,
		RefundedAt:    output.RefundedAt,
		FailureReason: output.FailureReason,
		Metadata:      output.Metadata,
	})
}

// RefundPayment initiates a refund for a payment.
// @Summary Refund payment
// @Description Initiates a refund for a payment
// @Tags integrations
// @Accept json
// @Produce json
// @Param storeId path string true "Store ID"
// @Param id path string true "Integration ID"
// @Param paymentId path string true "Payment ID"
// @Param body body RefundRequest true "Refund data"
// @Success 200 {object} httpx.Envelope{data=RefundResponse}
// @Failure 400 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/{id}/payments/{paymentId}/refund [post]
// @Security BearerAuth
func (h *Handler) RefundPayment(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")
	paymentID := c.Params("paymentId")

	var req RefundRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	output, err := h.service.RefundPayment(c.Context(), RefundPaymentInput{
		StoreID:       storeID,
		IntegrationID: id,
		PaymentID:     paymentID,
		Amount:        req.Amount,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, RefundResponse{
		RefundID:  output.RefundID,
		Status:    output.Status,
		Amount:    output.Amount,
		CreatedAt: output.CreatedAt,
	})
}

// =============================================================================
// OAUTH HANDLERS
// =============================================================================

// OAuthConnect initiates the OAuth flow for a provider.
// @Summary Start OAuth connection
// @Description Returns the OAuth authorization URL for the provider
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param provider path string true "Provider name (mercado_pago, tiny)"
// @Success 200 {object} httpx.Envelope{data=OAuthConnectResponse}
// @Failure 400 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/oauth/{provider}/connect [get]
// @Security BearerAuth
func (h *Handler) OAuthConnect(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	provider := c.Params("provider")

	output, err := h.service.GetOAuthURL(c.Context(), GetOAuthURLInput{
		StoreID:  storeID,
		Provider: provider,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, OAuthConnectResponse{
		AuthURL: output.AuthURL,
		State:   output.State,
	})
}

// GetProviderURLs returns the redirect (OAuth callback) and webhook URLs the
// merchant must paste into the provider's app config (e.g. Tiny).
// @Summary Get provider setup URLs
// @Description Returns the redirect URL and webhook URL the merchant must paste into the provider's app config
// @Tags integrations
// @Produce json
// @Param storeId path string true "Store ID"
// @Param provider path string true "Provider name (e.g. tiny, mercado_pago, pagarme)"
// @Success 200 {object} httpx.Envelope{data=ProviderURLsResponse}
// @Failure 422 {object} httpx.Envelope
// @Router /api/v1/stores/{storeId}/integrations/providers/{provider}/urls [get]
// @Security BearerAuth
func (h *Handler) GetProviderURLs(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	provider := c.Params("provider")

	output, err := h.service.GetProviderURLs(c.Context(), GetProviderURLsInput{
		StoreID:  storeID,
		Provider: provider,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, ProviderURLsResponse{
		Provider:    output.Provider,
		RedirectURL: output.RedirectURL,
		WebhookURL:  output.WebhookURL,
	})
}

// =============================================================================
// HELPERS
// =============================================================================

func mapToCredentials(m map[string]any) *providers.Credentials {
	if m == nil {
		return nil
	}

	creds := &providers.Credentials{
		Extra: make(map[string]any),
	}

	if v, ok := m["access_token"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := m["refresh_token"].(string); ok {
		creds.RefreshToken = v
	}
	if v, ok := m["token_type"].(string); ok {
		creds.TokenType = v
	}
	if v, ok := m["api_key"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := m["api_secret"].(string); ok {
		creds.APISecret = v
	}

	// Copy remaining fields to Extra
	for k, v := range m {
		switch k {
		case "access_token", "refresh_token", "token_type", "api_key", "api_secret", "expires_at":
			continue
		default:
			creds.Extra[k] = v
		}
	}

	return creds
}

func toIntegrationResponse(output *CreateIntegrationOutput) *IntegrationResponse {
	return &IntegrationResponse{
		ID:                output.ID,
		StoreID:           output.StoreID,
		Type:              output.Type,
		Provider:          output.Provider,
		Status:            output.Status,
		Metadata:          output.Metadata,
		LastSyncedAt:      output.LastSyncedAt,
		CreatedAt:         output.CreatedAt,
		RedirectURL:       output.RedirectURL,
		WebhookURL:        output.WebhookURL,
		WebhookStatus:     output.WebhookStatus,
		WebhookLastPingAt: output.WebhookLastPingAt,
		Priority:          output.Priority,
	}
}
