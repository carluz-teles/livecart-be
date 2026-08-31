package product

import (
	"strconv"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
	"livecart/apps/api/lib/storage"
	vo "livecart/apps/api/lib/valueobject"
)

type Handler struct {
	service  *Service
	s3Client *storage.S3Client
}

func NewHandler(service *Service, s3Client *storage.S3Client) *Handler {
	return &Handler{service: service, s3Client: s3Client}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/products")
	g.Get("/", h.List)
	g.Get("/stats", h.GetStats)
	g.Post("/", h.Create)
	g.Post("/upload-image", h.UploadImage)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
	g.Post("/:id/images", h.AddImage)
	g.Delete("/:id/images/:imageId", h.DeleteImage)
}

// UploadImage stores an uploaded image file in S3 and returns its permanent
// public URL, to be used as a product's imageUrl. Mirrors the store-logo upload
// but returns a public URL (products serve imageUrl as-is, everywhere) instead
// of a short-lived presigned one.
// @Summary      Upload a product image file
// @Tags         products
// @Accept       multipart/form-data
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        file formData file true "Image file (JPG, PNG, GIF, WebP; max 5MB)"
// @Success      200 {object} httpx.Envelope{data=UploadProductImageResponse}
// @Failure      400 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/products/upload-image [post]
// @Security     BearerAuth
func (h *Handler) UploadImage(c *fiber.Ctx) error {
	storeID := httpx.GetStoreID(c)
	if storeID == "" {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	if h.s3Client == nil {
		return httpx.ErrInternal("storage not configured")
	}

	file, err := c.FormFile("file")
	if err != nil {
		return httpx.ErrBadRequest("file is required")
	}
	if file.Size > 5*1024*1024 {
		return httpx.ErrBadRequest("file too large, maximum size is 5MB")
	}
	contentType := file.Header.Get("Content-Type")
	validTypes := map[string]bool{
		"image/jpeg": true,
		"image/jpg":  true,
		"image/png":  true,
		"image/gif":  true,
		"image/webp": true,
	}
	if !validTypes[contentType] {
		return httpx.ErrBadRequest("invalid file type, accepted: JPG, PNG, GIF, WebP")
	}

	src, err := file.Open()
	if err != nil {
		return httpx.ErrInternal("failed to read file")
	}
	defer src.Close()

	key, err := h.s3Client.UploadFile(c.UserContext(), src, file.Filename, contentType, "products/"+storeID)
	if err != nil {
		return httpx.ErrInternal("failed to upload file")
	}

	return httpx.OK(c, UploadProductImageResponse{URL: h.s3Client.GetPublicURL(key)})
}

// AddImage attaches one image URL to a variant gallery.
// @Summary      Attach image to product/variant
// @Tags         products
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Product UUID"
// @Param        request body AddProductImageRequest true "Image payload"
// @Success      201 {object} httpx.Envelope{data=AddProductImageResponse}
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/products/{id}/images [post]
// @Security     BearerAuth
func (h *Handler) AddImage(c *fiber.Ctx) error {
	var req AddProductImageRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewProductID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid product ID")
	}
	imageID, err := h.service.AddImage(c.UserContext(), id, storeID, req.URL, req.Position)
	if err != nil {
		return err
	}
	return httpx.Created(c, AddProductImageResponse{ID: imageID, URL: req.URL, Position: req.Position})
}

// DeleteImage removes one image from a variant gallery.
// @Summary      Detach image from product/variant
// @Tags         products
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Product UUID"
// @Param        imageId path string true "Image UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Router       /api/v1/stores/{storeId}/products/{id}/images/{imageId} [delete]
// @Security     BearerAuth
func (h *Handler) DeleteImage(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	productID, err := vo.NewProductID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid product ID")
	}
	imageIDStr := c.Params("imageId")
	imageID, err := vo.NewID(imageIDStr)
	if err != nil {
		return httpx.ErrUnprocessable("invalid image ID")
	}
	if err := h.service.DeleteImage(c.UserContext(), productID, storeID, imageID); err != nil {
		return err
	}
	return httpx.Deleted(c, imageIDStr)
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
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	product, err := h.service.Create(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewCreateProductResponse(product))
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewProductID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid product ID")
	}
	view, err := h.service.GetByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewProductResponse(view))
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}

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

	views, total, err := h.service.List(c.UserContext(), input)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewListProductsResponse(views, input.Pagination, total))
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
	var req UpdateProductRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), c.Params("id"))
	if err != nil {
		return err
	}
	view, err := h.service.Update(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewProductResponse(view))
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	id, err := vo.NewProductID(c.Params("id"))
	if err != nil {
		return httpx.ErrUnprocessable("invalid product ID")
	}
	if err := h.service.Delete(c.UserContext(), id, storeID); err != nil {
		return err
	}
	return httpx.Deleted(c, id.String())
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
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrUnprocessable("invalid store ID")
	}
	stats, err := h.service.GetStats(c.UserContext(), storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewProductStatsResponse(stats))
}
