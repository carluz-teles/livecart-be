package productgroup

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	productdomain "livecart/apps/api/internal/product/domain"
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
	g := router.Group("/product-groups")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
	g.Post("/:id/images", h.AddImage)
	g.Delete("/:id/images/:imageId", h.DeleteImage)
}

// Create godoc
// @Summary      Create a product group with variants
// @Description  Creates the aggregator (group), its options/values, and N variants atomically.
// @Tags         product-groups
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateGroupRequest true "Group payload"
// @Success      201 {object} httpx.Envelope{data=CreateGroupResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/product-groups [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
	var req CreateGroupRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}

	source, err := productdomain.NewExternalSource(req.ExternalSource)
	if err != nil {
		return httpx.BadRequest(c, "invalid external source")
	}

	out, err := h.service.Create(c.Context(), CreateGroupInput{
		StoreID:        storeID,
		Name:           req.Name,
		Description:    req.Description,
		ExternalID:     req.ExternalID,
		ExternalSource: source,
		Options:        req.Options,
		GroupImages:    req.GroupImages,
		Variants:       req.Variants,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, out)
}

// GetByID godoc
// @Summary      Get product group detail
// @Tags         product-groups
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Group UUID"
// @Success      200 {object} httpx.Envelope{data=GroupDetailResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/product-groups/{id} [get]
// @Security     BearerAuth
func (h *Handler) GetByID(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.BadRequest(c, "invalid group ID")
	}
	out, err := h.service.GetByID(c.Context(), id, storeID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

// List godoc
// @Summary      List product groups
// @Tags         product-groups
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        page query int false "Page" default(1)
// @Param        limit query int false "Items per page" default(20)
// @Success      200 {object} httpx.Envelope{data=ListGroupsResponse}
// @Router       /api/v1/stores/{storeId}/product-groups [get]
// @Security     BearerAuth
func (h *Handler) List(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	pag := query.Pagination{
		Page:  c.QueryInt("page", query.DefaultPage),
		Limit: c.QueryInt("limit", query.DefaultLimit),
	}
	pag.Normalize()

	groups, total, err := h.service.List(c.Context(), storeID, pag.Limit, pag.Offset())
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, ListGroupsResponse{
		Data:       groups,
		Pagination: query.NewPaginationResponse(pag, total),
	})
}

// Update godoc
// @Summary      Update product group (name/description)
// @Tags         product-groups
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Group UUID"
// @Param        request body UpdateGroupRequest true "Group update"
// @Success      200 {object} httpx.Envelope{data=GroupDetailResponse}
// @Router       /api/v1/stores/{storeId}/product-groups/{id} [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	var req UpdateGroupRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.BadRequest(c, "invalid group ID")
	}
	out, err := h.service.Update(c.Context(), id, storeID, req.Name, req.Description)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

// Delete godoc
// @Summary      Delete a product group (variants become unlinked)
// @Tags         product-groups
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Group UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Router       /api/v1/stores/{storeId}/product-groups/{id} [delete]
// @Security     BearerAuth
func (h *Handler) Delete(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	idStr := c.Params("id")
	id, err := vo.NewID(idStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid group ID")
	}
	if err := h.service.Delete(c.Context(), id, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Deleted(c, idStr)
}

// AddImage godoc
// @Summary      Add an image to the group gallery
// @Tags         product-groups
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Group UUID"
// @Param        request body AddImageRequest true "Image payload"
// @Success      201 {object} httpx.Envelope{data=ImageResponse}
// @Router       /api/v1/stores/{storeId}/product-groups/{id}/images [post]
// @Security     BearerAuth
func (h *Handler) AddImage(c *fiber.Ctx) error {
	var req AddImageRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.BadRequest(c, "invalid group ID")
	}
	out, err := h.service.AddGroupImage(c.Context(), id, storeID, req.URL, req.Position)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, out)
}

// DeleteImage godoc
// @Summary      Remove an image from the group gallery
// @Tags         product-groups
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Group UUID"
// @Param        imageId path string true "Image UUID"
// @Success      200 {object} httpx.Envelope{data=httpx.DeletedResponse}
// @Router       /api/v1/stores/{storeId}/product-groups/{id}/images/{imageId} [delete]
// @Security     BearerAuth
func (h *Handler) DeleteImage(c *fiber.Ctx) error {
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.BadRequest(c, "invalid store ID")
	}
	groupID, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.BadRequest(c, "invalid group ID")
	}
	imageIDStr := c.Params("imageId")
	imageID, err := vo.NewID(imageIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid image ID")
	}
	if err := h.service.DeleteGroupImage(c.Context(), imageID, groupID, storeID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Deleted(c, imageIDStr)
}
