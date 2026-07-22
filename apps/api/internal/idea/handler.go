package idea

import (
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes mounts the ideas channel under the given (already-authed)
// router. Routes are NOT store-scoped — ideas are global across all stores.
func (h *Handler) RegisterRoutes(router fiber.Router) {
	router.Get("/idea-categories", h.ListCategories)

	g := router.Group("/ideas")
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.GetByID)
	g.Post("/:id/vote", h.ToggleVote)
	g.Post("/:id/comments", h.CreateComment)
}

// ListCategories godoc
// @Summary      List idea categories
// @Description  Returns the catalog of idea categories (slug + label)
// @Tags         ideas
// @Produce      json
// @Success      200  {object}  httpx.Envelope{data=[]Category}
// @Security     BearerAuth
// @Router       /api/v1/idea-categories [get]
func (h *Handler) ListCategories(c *fiber.Ctx) error {
	// httpx.OK already wraps the value in {data: ...}; sending the slice
	// directly avoids the double-nested {data: {data: [...]}} that breaks
	// the frontend client (which only unwraps one layer).
	return httpx.OK(c, Categories)
}

// List godoc
// @Summary      List ideas feed
// @Description  Returns a paginated, filterable feed of ideas
// @Tags         ideas
// @Produce      json
// @Param        tab       query     string  false  "Feed tab"
// @Param        category  query     string  false  "Category slug filter"
// @Param        q         query     string  false  "Search query"
// @Param        sort      query     string  false  "Sort order"
// @Param        page      query     int     false  "Page number"
// @Param        limit     query     int     false  "Page size"
// @Success      200       {object}  ListIdeasResponse
// @Failure      401       {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/ideas [get]
func (h *Handler) List(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	in := ListIdeasInput{
		UserID:   userID,
		Tab:      c.Query("tab", TabAll),
		Category: c.Query("category"),
		Search:   c.Query("q"),
		Sort:     c.Query("sort", SortTrending),
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
	}

	ideas, total, err := h.service.List(c.UserContext(), in)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewListIdeasResponse(ideas, userID, in.Pagination, total))
}

// Create godoc
// @Summary      Create idea
// @Description  Proposes a new idea in the ideas channel
// @Tags         ideas
// @Accept       json
// @Produce      json
// @Param        body  body      CreateIdeaRequest  true  "Idea to create"
// @Success      201   {object}  httpx.Envelope{data=IdeaResponse}
// @Failure      400   {object}  httpx.Envelope
// @Failure      401   {object}  httpx.Envelope
// @Failure      422   {object}  httpx.ValidationEnvelope
// @Security     BearerAuth
// @Router       /api/v1/ideas [post]
func (h *Handler) Create(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	var req CreateIdeaRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(userID)
	if err != nil {
		return err
	}

	created, err := h.service.Create(c.UserContext(), input)
	if err != nil {
		return err
	}

	return httpx.Created(c, NewIdeaResponse(created, userID))
}

// GetByID godoc
// @Summary      Get idea detail
// @Description  Returns a single idea with its threaded comments
// @Tags         ideas
// @Produce      json
// @Param        id   path      string  true  "Idea ID"
// @Success      200  {object}  httpx.Envelope{data=IdeaDetailResponse}
// @Failure      401  {object}  httpx.Envelope
// @Failure      404  {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/ideas/{id} [get]
func (h *Handler) GetByID(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	detail, err := h.service.GetDetail(c.UserContext(), c.Params("id"), userID)
	if err != nil {
		return err
	}

	return httpx.OK(c, NewIdeaDetailResponse(detail, userID))
}

// ToggleVote godoc
// @Summary      Toggle idea vote
// @Description  Adds or removes the caller's vote on an idea
// @Tags         ideas
// @Produce      json
// @Param        id   path      string  true  "Idea ID"
// @Success      200  {object}  httpx.Envelope{data=ToggleVoteResponse}
// @Failure      401  {object}  httpx.Envelope
// @Failure      403  {object}  httpx.Envelope
// @Failure      404  {object}  httpx.Envelope
// @Security     BearerAuth
// @Router       /api/v1/ideas/{id}/vote [post]
func (h *Handler) ToggleVote(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	result, err := h.service.ToggleVote(c.UserContext(), c.Params("id"), userID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewToggleVoteResponse(result))
}

// CreateComment godoc
// @Summary      Comment on idea
// @Description  Posts a comment or reply on an idea
// @Tags         ideas
// @Accept       json
// @Produce      json
// @Param        id    path      string                true  "Idea ID"
// @Param        body  body      CreateCommentRequest  true  "Comment to create"
// @Success      201   {object}  httpx.Envelope{data=CommentResponse}
// @Failure      400   {object}  httpx.Envelope
// @Failure      401   {object}  httpx.Envelope
// @Failure      404   {object}  httpx.Envelope
// @Failure      422   {object}  httpx.ValidationEnvelope
// @Security     BearerAuth
// @Router       /api/v1/ideas/{id}/comments [post]
func (h *Handler) CreateComment(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	var req CreateCommentRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(c.Params("id"), userID)
	if err != nil {
		return err
	}

	comment, err := h.service.CreateComment(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.Created(c, NewCommentResponse(comment))
}
