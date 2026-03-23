package product

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

type Handler struct {
	service  *Service
	validate *validator.Validate
}

func NewHandler(service *Service, validate *validator.Validate) *Handler {
	return &Handler{service: service, validate: validate}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/products")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
}

// Create godoc
// @Summary      Create a new product
// @Description  Creates a product for the current store. Keyword is auto-generated if not provided.
// @Tags         products
// @Accept       json
// @Produce      json
// @Param        request body CreateProductRequest true "Product creation payload"
// @Success      201 {object} httpx.Envelope{data=CreateProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/products [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	storeID := c.Locals("store_id").(string)

	output, err := h.service.Create(c.Context(), CreateProductInput{
		StoreID:        storeID,
		Name:           req.Name,
		ExternalID:     req.ExternalID,
		ExternalSource: req.ExternalSource,
		Keyword:        req.Keyword,
		Price:          req.Price,
		ImageURL:       req.ImageURL,
		Sizes:          req.Sizes,
		Stock:          req.Stock,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, CreateProductResponse{
		ID:        output.ID,
		Name:      output.Name,
		Keyword:   output.Keyword,
		CreatedAt: output.CreatedAt,
	})
}

// GetByID godoc
// @Summary      Get product by ID
// @Description  Returns a single product by its UUID
// @Tags         products
// @Produce      json
// @Param        id path string true "Product UUID"
// @Success      200 {object} httpx.Envelope{data=ProductResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/products/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toProductResponse(output))
}

// List godoc
// @Summary      List products
// @Description  Returns all products for the current store
// @Tags         products
// @Produce      json
// @Success      200 {object} httpx.Envelope{data=[]ProductResponse}
// @Router       /api/v1/products [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)

	outputs, err := h.service.List(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]ProductResponse, len(outputs))
	for i, o := range outputs {
		responses[i] = toProductResponse(o)
	}

	return httpx.OK(c, responses)
}

// Update godoc
// @Summary      Update a product
// @Description  Updates an existing product by its UUID
// @Tags         products
// @Accept       json
// @Produce      json
// @Param        id path string true "Product UUID"
// @Param        request body UpdateProductRequest true "Product update payload"
// @Success      200 {object} httpx.Envelope{data=ProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/products/{id} [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	id := c.Params("id")

	var req UpdateProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	output, err := h.service.Update(c.Context(), UpdateProductInput{
		StoreID:  storeID,
		ID:       id,
		Name:     req.Name,
		Price:    req.Price,
		ImageURL: req.ImageURL,
		Sizes:    req.Sizes,
		Stock:    req.Stock,
		Active:   req.Active,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toProductResponse(output))
}

func toProductResponse(o ProductOutput) ProductResponse {
	return ProductResponse{
		ID:             o.ID,
		Name:           o.Name,
		ExternalID:     o.ExternalID,
		ExternalSource: o.ExternalSource,
		Keyword:        o.Keyword,
		Price:          o.Price,
		ImageURL:       o.ImageURL,
		Sizes:          o.Sizes,
		Stock:          o.Stock,
		Active:         o.Active,
		CreatedAt:      o.CreatedAt,
		UpdatedAt:      o.UpdatedAt,
	}
}
