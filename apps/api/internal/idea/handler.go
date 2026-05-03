package idea

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

func (h *Handler) ListCategories(c *fiber.Ctx) error {
	// httpx.OK already wraps the value in {data: ...}; sending the slice
	// directly avoids the double-nested {data: {data: [...]}} that breaks
	// the frontend client (which only unwraps one layer).
	return httpx.OK(c, Categories)
}

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

	out, err := h.service.List(c.Context(), in)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	resp := make([]IdeaListItemResponse, len(out.Items))
	for i, it := range out.Items {
		resp[i] = toListItemResponse(it, userID)
	}

	return httpx.OK(c, ListIdeasResponse{
		Data:       resp,
		Pagination: query.NewPaginationResponse(out.Pagination, out.Total),
	})
}

func (h *Handler) Create(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	var req CreateIdeaRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	created, err := h.service.Create(c.Context(), userID, req)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.Created(c, toListItemResponse(*created, userID))
}

func (h *Handler) GetByID(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	id := c.Params("id")

	detail, err := h.service.GetDetail(c.Context(), id, userID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, IdeaDetailResponse{
		IdeaListItemResponse: toListItemResponse(detail.IdeaListItem, userID),
		Comments:             toCommentNodes(detail.Comments),
	})
}

func (h *Handler) ToggleVote(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	id := c.Params("id")

	out, err := h.service.ToggleVote(c.Context(), id, userID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

func (h *Handler) CreateComment(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	id := c.Params("id")

	var req CreateCommentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return httpx.ValidationError(c, err)
	}

	out, err := h.service.CreateComment(c.Context(), id, userID, req)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.Created(c, out)
}

func toListItemResponse(it IdeaListItem, callerID string) IdeaListItemResponse {
	label := it.Category
	for _, cat := range Categories {
		if cat.Slug == it.Category {
			label = cat.Label
			break
		}
	}
	return IdeaListItemResponse{
		ID:            it.ID,
		Number:        it.Number,
		Title:         it.Title,
		Description:   it.Description,
		Category:      it.Category,
		CategoryLabel: label,
		Status:        it.Status,
		AuthorID:      it.AuthorID,
		AuthorName:    it.AuthorName,
		VoteCount:     it.VoteCount,
		CommentCount:  it.CommentCount,
		VotedByMe:     it.VotedByMe,
		IsAuthor:      it.AuthorID == callerID,
		CreatedAt:     it.CreatedAt,
	}
}

func toCommentNodes(in []CommentNode) []CommentNodeResponse {
	out := make([]CommentNodeResponse, len(in))
	for i, n := range in {
		out[i] = CommentNodeResponse{
			ID:         n.ID,
			Body:       n.Body,
			AuthorID:   n.AuthorID,
			AuthorName: n.AuthorName,
			CreatedAt:  n.CreatedAt,
			Replies:    toCommentNodes(n.Replies),
		}
	}
	return out
}
