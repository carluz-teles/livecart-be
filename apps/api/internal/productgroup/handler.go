package productgroup

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
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
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	result, err := h.service.Create(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewCreateGroupResponse(result))
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
		return httpx.ErrBadRequest("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid group ID")
	}
	detail, err := h.service.GetByID(c.UserContext(), id, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewGroupDetailResponse(detail))
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
		return httpx.ErrBadRequest("invalid store ID")
	}
	pag := query.Pagination{
		Page:  c.QueryInt("page", query.DefaultPage),
		Limit: c.QueryInt("limit", query.DefaultLimit),
	}
	pag.Normalize()

	groups, total, err := h.service.List(c.UserContext(), storeID, pag.Limit, pag.Offset())
	if err != nil {
		return err
	}
	return httpx.OK(c, NewListGroupsResponse(groups, query.NewPaginationResponse(pag, total)))
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
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/product-groups/{id} [put]
// @Security     BearerAuth
func (h *Handler) Update(c *fiber.Ctx) error {
	var req UpdateGroupRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid group ID")
	}
	detail, err := h.service.Update(c.UserContext(), id, storeID, req.Name, req.Description)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewGroupDetailResponse(detail))
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
		return httpx.ErrBadRequest("invalid store ID")
	}
	idStr := c.Params("id")
	id, err := vo.NewID(idStr)
	if err != nil {
		return httpx.ErrBadRequest("invalid group ID")
	}
	if err := h.service.Delete(c.UserContext(), id, storeID); err != nil {
		return err
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
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/product-groups/{id}/images [post]
// @Security     BearerAuth
func (h *Handler) AddImage(c *fiber.Ctx) error {
	var req AddImageRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	storeID, err := vo.NewStoreID(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store ID")
	}
	id, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid group ID")
	}
	img, err := h.service.AddGroupImage(c.UserContext(), id, storeID, req.URL, req.Position)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewImageResponse(img))
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
		return httpx.ErrBadRequest("invalid store ID")
	}
	groupID, err := vo.NewID(c.Params("id"))
	if err != nil {
		return httpx.ErrBadRequest("invalid group ID")
	}
	imageIDStr := c.Params("imageId")
	imageID, err := vo.NewID(imageIDStr)
	if err != nil {
		return httpx.ErrBadRequest("invalid image ID")
	}
	if err := h.service.DeleteGroupImage(c.UserContext(), imageID, groupID, storeID); err != nil {
		return err
	}
	return httpx.Deleted(c, imageIDStr)
}
