package product

import (
	"strconv"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
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
	g.Get("/stats", h.GetStats)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
}

// Create godoc
// @Summary      Create a new product
// @Description  Creates a product for the current store. Keyword is auto-generated if not provided.
// @Tags         products
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateProductRequest true "Product creation payload"
// @Success      201 {object} httpx.Envelope{data=CreateProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/products [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	storeIDStr := httpx.GetStoreID(c)

	// Convert to value objects
	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	externalSource, err := domain.NewExternalSource(req.ExternalSource)
	if err != nil {
		return httpx.BadRequest(c, "invalid external source")
	}

	price, err := vo.NewMoney(req.Price)
	if err != nil {
		return httpx.BadRequest(c, "invalid price")
	}

	output, err := h.service.Create(c.Context(), CreateProductInput{
		StoreID:        storeID,
		Name:           req.Name,
		ExternalID:     req.ExternalID,
		ExternalSource: externalSource,
		Keyword:        req.Keyword,
		Price:          price,
		ImageURL:       req.ImageURL,
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
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Product UUID"
// @Success      200 {object} httpx.Envelope{data=ProductResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/products/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	idStr := c.Params("id")

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	id, err := vo.NewProductID(idStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid product ID")
	}

	output, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toProductResponse(output))
}

// List godoc
// @Summary      List products
// @Description  Returns products with filtering, pagination, and sorting
// @Tags         products
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        search query string false "Search by name or keyword"
// @Param        page query int false "Page number" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Param        sortBy query string false "Sort field" default(created_at)
// @Param        sortOrder query string false "Sort order (asc, desc)" default(desc)
// @Param        status query []string false "Filter by status (active, inactive)"
// @Param        externalSource query []string false "Filter by source (manual, bling, tiny, shopify)"
// @Param        priceMin query int false "Minimum price in cents"
// @Param        priceMax query int false "Maximum price in cents"
// @Param        stockMin query int false "Minimum stock"
// @Param        stockMax query int false "Maximum stock"
// @Param        hasLowStock query bool false "Filter low stock (<=5)"
// @Success      200 {object} httpx.Envelope{data=ListProductsResponse}
// @Router       /api/v1/stores/{storeId}/products [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	// Parse query parameters
	input := ListProductsInput{
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
		Filters: parseProductFilters(c),
	}

	output, err := h.service.List(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	responses := make([]ProductResponse, len(output.Products))
	for i, o := range output.Products {
		responses[i] = toProductResponse(o)
	}

	return httpx.OK(c, ListProductsResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(output.Pagination, output.Total),
	})
}

func parseProductFilters(c *fiber.Ctx) ProductFilters {
	var filters ProductFilters

	// Parse status filter (multi-value)
	statusBytes := c.Context().QueryArgs().PeekMulti("status")
	if len(statusBytes) > 0 {
		filters.Status = make([]string, len(statusBytes))
		for i, s := range statusBytes {
			filters.Status[i] = string(s)
		}
	}

	// Parse external source filter (multi-value)
	sourceBytes := c.Context().QueryArgs().PeekMulti("externalSource")
	if len(sourceBytes) > 0 {
		filters.ExternalSource = make([]string, len(sourceBytes))
		for i, s := range sourceBytes {
			filters.ExternalSource[i] = string(s)
		}
	}

	// Parse numeric filters
	if priceMin := c.Query("priceMin"); priceMin != "" {
		if v, err := strconv.ParseInt(priceMin, 10, 64); err == nil {
			filters.PriceMin = &v
		}
	}
	if priceMax := c.Query("priceMax"); priceMax != "" {
		if v, err := strconv.ParseInt(priceMax, 10, 64); err == nil {
			filters.PriceMax = &v
		}
	}
	if stockMin := c.Query("stockMin"); stockMin != "" {
		if v, err := strconv.Atoi(stockMin); err == nil {
			filters.StockMin = &v
		}
	}
	if stockMax := c.Query("stockMax"); stockMax != "" {
		if v, err := strconv.Atoi(stockMax); err == nil {
			filters.StockMax = &v
		}
	}
	if hasLowStock := c.Query("hasLowStock"); hasLowStock == "true" {
		v := true
		filters.HasLowStock = &v
	}

	return filters
}

// Update godoc
// @Summary      Update a product
// @Description  Updates an existing product by its UUID
// @Tags         products
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Product UUID"
// @Param        request body UpdateProductRequest true "Product update payload"
// @Success      200 {object} httpx.Envelope{data=ProductResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      404 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/products/{id} [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	idStr := c.Params("id")

	var req UpdateProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	id, err := vo.NewProductID(idStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid product ID")
	}

	price, err := vo.NewMoney(req.Price)
	if err != nil {
		return httpx.BadRequest(c, "invalid price")
	}

	output, err := h.service.Update(c.Context(), UpdateProductInput{
		StoreID:  storeID,
		ID:       id,
		Name:     req.Name,
		Price:    price,
		ImageURL: req.ImageURL,
		Stock:    req.Stock,
		Active:   req.Active,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, toProductResponse(output))
}

// Delete godoc
// @Summary      Delete a product
// @Description  Deletes a product by its UUID
// @Tags         products
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Product UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/products/{id} [delete]
// @Security     BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)
	idStr := c.Params("id")

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	id, err := vo.NewProductID(idStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid product ID")
	}

	if err := h.service.Delete(c.Context(), id, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Deleted(c, idStr)
}

// GetStats godoc
// @Summary      Get product statistics
// @Description  Returns aggregated statistics for all products in the store
// @Tags         products
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=ProductStatsResponse}
// @Router       /api/v1/stores/{storeId}/products/stats [get]
// @Security     BearerAuth
func (h *Handler) GetStats(c *fiber.Ctx) error {
	storeIDStr := httpx.GetStoreID(c)

	storeID, err := vo.NewStoreID(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	output, err := h.service.GetStats(c.Context(), storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, ProductStatsResponse{
		TotalProducts: output.TotalProducts,
		ActiveCount:   output.ActiveCount,
		LowStockCount: output.LowStockCount,
		StockValue:    output.StockValue,
	})
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
		Stock:          o.Stock,
		Active:         o.Active,
		CreatedAt:      o.CreatedAt,
		UpdatedAt:      o.UpdatedAt,
	}
}
